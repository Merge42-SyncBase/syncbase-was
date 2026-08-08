package webapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/documents"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func TestFailedFirstVersionShowsActionableNonProcessingState(t *testing.T) {
	templates, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	documentID := uuid.New()
	runID := uuid.New()
	document := knowledge.DocumentDetails{
		ID: documentID, Name: "실패 문서", Versions: []knowledge.VersionView{{
			ID: uuid.New(), VersionNumber: 1, Status: knowledge.VersionFailed,
			Stage: knowledge.StageParse, RunID: runID, ErrorCode: "INVALID_INPUT",
			AutomaticAttempts: 1, CorrelationID: "corr-invalid",
		}},
	}
	var output bytes.Buffer
	if err := templates.ExecuteTemplate(&output, "details", map[string]any{
		"Document": document, "Processing": false, "CSRF": "csrf",
	}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	if !strings.Contains(output.String(), "검색 반영이 필요합니다") ||
		strings.Contains(output.String(), "첫 버전을 처리하고 있습니다") ||
		strings.Contains(output.String(), "처리 재시도") ||
		!strings.Contains(output.String(), "corr-invalid") {
		t.Fatalf("permanent failure detail is misleading: %s", output.String())
	}
	document.Versions[0].ErrorCode = "TRANSIENT_EXHAUSTED"
	document.Versions[0].AutomaticAttempts = 3
	document.Versions[0].ManualRetryAllowed = true
	output.Reset()
	if err := templates.ExecuteTemplate(&output, "details", map[string]any{
		"Document": document, "Processing": false, "CSRF": "csrf",
	}); err != nil {
		t.Fatalf("ExecuteTemplate retryable: %v", err)
	}
	if !strings.Contains(output.String(), "처리 재시도") || !strings.Contains(output.String(), "3 / 3") {
		t.Fatalf("exhausted transient failure lacks recovery action: %s", output.String())
	}
}

func TestSourceTemplateUsesPinnedPDFJSCanvasAndTextLayer(t *testing.T) {
	templates, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	source := knowledge.SourceDocument{
		DocumentID: uuid.New(), Name: "운영 정책", Version: 2, PageCount: 3,
	}
	var output bytes.Buffer
	if err := templates.ExecuteTemplate(&output, "source", map[string]any{
		"Source": source, "Page": 2, "SourceURL": "/source-url",
	}); err != nil {
		t.Fatalf("ExecuteTemplate: %v", err)
	}
	body := output.String()
	for _, required := range []string{"pdf-canvas", "text-layer", "/vendor/pdfjs/pdf.mjs", "data-page=\"2\""} {
		if !strings.Contains(body, required) {
			t.Fatalf("source viewer missing %q: %s", required, body)
		}
	}
	if strings.Contains(body, `class="native-pdf"`) {
		t.Fatalf("source viewer still uses native PDF iframe: %s", body)
	}
}

func TestAdminLoginEntersRealDocumentLibrary(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	documentID := uuid.New()
	handler, err := New(Config{
		AdminUsername:       "admin",
		AdminPasswordBcrypt: string(hash),
		StaticRoot:          "../../web/static",
	}, &fixtureDocumentStore{documents: []knowledge.DocumentSummary{{
		ID:            documentID,
		Name:          "정보보안 정책",
		LatestVersion: 1,
		LatestStatus:  knowledge.VersionQueued,
	}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := server.Client()
	client.Jar = jar

	response, err := client.PostForm(server.URL+"/login", url.Values{
		"username": {"admin"},
		"password": {"correct horse battery staple"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Request.URL.Path != "/documents" {
		t.Fatalf("login final status=%d path=%s", response.StatusCode, response.Request.URL.Path)
	}
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(content), "정보보안 정책") || !strings.Contains(string(content), documentID.String()) {
		t.Fatalf("document library did not render real data: %s", content)
	}
}

func TestFailedLoginPreservesUsername(t *testing.T) {
	handler := newTestHandler(t, &fixtureDocumentStore{})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	response, err := server.Client().PostForm(server.URL+"/login", url.Values{
		"username": {"ops-admin"}, "password": {"wrong"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read login response: %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(string(content), `value="ops-admin"`) {
		t.Fatalf("status=%d body=%s", response.StatusCode, content)
	}
}

func TestLoginRateLimitBoundsRepeatedFailures(t *testing.T) {
	handler := newTestHandler(t, &fixtureDocumentStore{})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	for attempt := 1; attempt <= loginFailureLimit; attempt++ {
		response, err := server.Client().PostForm(server.URL+"/login", url.Values{
			"username": {"admin"}, "password": {"wrong"},
		})
		if err != nil {
			t.Fatalf("login attempt %d: %v", attempt, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login attempt %d status = %d, want %d", attempt, response.StatusCode, http.StatusUnauthorized)
		}
	}
	response, err := server.Client().PostForm(server.URL+"/login", url.Values{
		"username": {"admin"}, "password": {"wrong"},
	})
	if err != nil {
		t.Fatalf("rate-limited login: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") == "" {
		t.Fatalf("rate-limited status=%d Retry-After=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}
}

func TestSafeNextAcceptsOnlyInternalAbsolutePaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  string
	}{
		{value: "/documents?after=10", want: "/documents?after=10"},
		{value: "documents", want: ""},
		{value: "//evil.example/path", want: ""},
		{value: "/\\evil.example/path", want: ""},
		{value: "/%5c%5cevil.example/path", want: ""},
		{value: "/%2f%2fevil.example/path", want: ""},
		{value: "/documents\r\nLocation: https://evil.example", want: ""},
		{value: "https://evil.example/path", want: ""},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			if got := safeNext(test.value); got != test.want {
				t.Fatalf("safeNext(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestUploadFetchWithoutSessionReturnsJSONLoginRecovery(t *testing.T) {
	handler := newTestHandler(t, &fixtureDocumentStore{})
	request := httptest.NewRequest(http.MethodPost, "/api/uploads/preflight", strings.NewReader(""))
	request.Header.Set("Accept", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("Location") != "" ||
		!strings.Contains(response.Body.String(), `"status":"session_expired"`) ||
		!strings.Contains(response.Body.String(), `"loginUrl"`) {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestReadinessDistinguishesLivenessFromDependencyFailure(t *testing.T) {
	t.Parallel()

	store := &fixtureDocumentStore{readyErr: errors.New("database unavailable")}
	handler := newTestHandler(t, store)
	for _, test := range []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/healthz", wantStatus: http.StatusOK, wantBody: `"status":"ok"`},
		{path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `"status":"not_ready"`},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantBody) {
			t.Errorf("GET %s: status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
}

func newTestHandler(t *testing.T, documents documentService) http.Handler {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	handler, err := New(Config{
		AdminUsername: "admin", AdminPasswordBcrypt: string(hash), StaticRoot: "../../web/static",
	}, documents)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler
}

type fixtureDocumentStore struct {
	documents []knowledge.DocumentSummary
	readyErr  error
}

func (s *fixtureDocumentStore) ListDocuments(context.Context, int, int) ([]knowledge.DocumentSummary, error) {
	return s.documents, nil
}

func (s *fixtureDocumentStore) GetDocument(context.Context, uuid.UUID) (knowledge.DocumentDetails, error) {
	return knowledge.DocumentDetails{}, knowledge.ErrNotFound
}

func (s *fixtureDocumentStore) Preflight(context.Context, string, []byte) (documents.Preflight, error) {
	return documents.Preflight{}, knowledge.ErrInvalidPDF
}

func (s *fixtureDocumentStore) RecoverRegistration(context.Context, string) (knowledge.UploadRecovery, error) {
	return knowledge.UploadRecovery{State: knowledge.UploadNotCommitted}, nil
}

func (s *fixtureDocumentStore) Source(context.Context, uuid.UUID, int) (documents.Source, error) {
	return documents.Source{}, knowledge.ErrNotFound
}

func (s *fixtureDocumentStore) Register(context.Context, documents.RegisterCommand) (knowledge.Registration, error) {
	return knowledge.Registration{}, knowledge.ErrInvalidArgument
}

func (s *fixtureDocumentStore) Retry(context.Context, uuid.UUID, string) (uuid.UUID, error) {
	return uuid.Nil, knowledge.ErrInvalidArgument
}

func (s *fixtureDocumentStore) Ready(context.Context) error {
	return s.readyErr
}
