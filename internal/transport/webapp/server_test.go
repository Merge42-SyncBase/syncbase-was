package webapp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/documents"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/sessions"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func TestLoginRateLimitBoundsRepeatedFailures(t *testing.T) {
	handler := newTestHandler(t, &fixtureDocumentStore{})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	for attempt := 1; attempt <= loginFailureLimit; attempt++ {
		response, err := server.Client().Post(server.URL+"/api/v1/session", "application/json", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		if err != nil {
			t.Fatalf("login attempt %d: %v", attempt, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login attempt %d status=%d", attempt, response.StatusCode)
		}
	}
	response, err := server.Client().Post(server.URL+"/api/v1/session", "application/json", strings.NewReader(`{"username":"admin","password":"wrong"}`))
	if err != nil {
		t.Fatalf("rate-limited login: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") == "" {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
	}
}

func TestReadinessDistinguishesLivenessFromDependencyFailure(t *testing.T) {
	handler := newTestHandler(t, &fixtureDocumentStore{readyErr: context.DeadlineExceeded})
	for _, test := range []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/healthz", wantStatus: http.StatusOK, wantBody: `"status":"ok"`},
		{path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `"code":"NOT_READY"`},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantBody) {
			t.Errorf("GET %s: status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
}

func TestSessionSurvivesWebHandlerReplacement(t *testing.T) {
	store := newMemorySessionStore()
	first := newTestHandlerWithSessionStore(t, &fixtureDocumentStore{}, store)
	loginRequest := httptest.NewRequest(
		http.MethodPost, "/api/v1/session",
		strings.NewReader(`{"username":"admin","password":"correct horse battery staple"}`),
	)
	loginRequest.Header.Set("Content-Type", "application/json")
	loginResponse := httptest.NewRecorder()
	first.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, loginResponse.Body.String())
	}
	cookies := loginResponse.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies=%d, want 1", len(cookies))
	}

	second := newTestHandlerWithSessionStore(t, &fixtureDocumentStore{}, store)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	request.AddCookie(cookies[0])
	response := httptest.NewRecorder()
	second.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("session after handler replacement status=%d body=%s", response.Code, response.Body.String())
	}
}

func newTestHandler(t *testing.T, documentStore documentService) http.Handler {
	return newTestHandlerWithSessionStore(t, documentStore, newMemorySessionStore())
}

func newTestHandlerWithSessionStore(t *testing.T, documentStore documentService, store sessions.Store) http.Handler {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	handler, err := New(Config{
		AdminUsername: "admin", AdminPasswordBcrypt: string(hash), Sessions: store,
	}, documentStore)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler
}

type memorySessionStore struct {
	mu      sync.Mutex
	records map[string]sessions.Record
}

func newMemorySessionStore() *memorySessionStore {
	return &memorySessionStore{records: make(map[string]sessions.Record)}
}

func (s *memorySessionStore) Create(_ context.Context, token string, record sessions.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[token] = record
	return nil
}

func (s *memorySessionStore) Load(_ context.Context, token string, now time.Time) (sessions.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found := s.records[token]
	if !found || !record.ExpiresAt.After(now) {
		delete(s.records, token)
		return sessions.Record{}, false, nil
	}
	return record, true, nil
}

func (s *memorySessionStore) Delete(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, token)
	return nil
}

type fixtureDocumentStore struct {
	documents []knowledge.DocumentSummary
	readyErr  error
}

func (s *fixtureDocumentStore) ListDocuments(context.Context, int, int) ([]knowledge.DocumentSummary, error) {
	return s.documents, nil
}
func (*fixtureDocumentStore) FindNameMatches(context.Context, string, int) (documents.NameMatches, error) {
	return documents.NameMatches{}, nil
}
func (*fixtureDocumentStore) GetDocument(context.Context, uuid.UUID) (knowledge.DocumentDetails, error) {
	return knowledge.DocumentDetails{}, knowledge.ErrNotFound
}
func (*fixtureDocumentStore) Preflight(context.Context, string, []byte) (documents.Preflight, error) {
	return documents.Preflight{}, knowledge.ErrInvalidPDF
}
func (*fixtureDocumentStore) RecoverRegistration(context.Context, string) (knowledge.UploadRecovery, error) {
	return knowledge.UploadRecovery{State: knowledge.UploadNotCommitted}, nil
}
func (*fixtureDocumentStore) Source(context.Context, uuid.UUID, int) (documents.Source, error) {
	return documents.Source{}, knowledge.ErrNotFound
}
func (*fixtureDocumentStore) Register(context.Context, documents.RegisterCommand) (knowledge.Registration, error) {
	return knowledge.Registration{}, knowledge.ErrInvalidArgument
}
func (*fixtureDocumentStore) Retry(context.Context, uuid.UUID, string) (uuid.UUID, error) {
	return uuid.Nil, knowledge.ErrInvalidArgument
}
func (s *fixtureDocumentStore) Ready(context.Context) error { return s.readyErr }
