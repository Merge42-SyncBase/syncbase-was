package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/documents"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
)

func TestAPIAdminSessionAndDocumentProjection(t *testing.T) {
	documentID := uuid.New()
	versionID := uuid.New()
	updatedAt := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	store := &apiFixtureDocumentStore{
		list: []knowledge.DocumentSummary{{
			ID: documentID, Name: "정보보안 정책", LatestVersion: 2,
			LatestStatus: knowledge.VersionActive, ActiveVersion: pointer(2), UpdatedAt: updatedAt,
		}},
		details: knowledge.DocumentDetails{
			ID: documentID, Name: "정보보안 정책", ActiveVersion: pointer(2),
			Versions: []knowledge.VersionView{{
				ID: versionID, VersionNumber: 2, Status: knowledge.VersionActive, Active: true,
				Stage: knowledge.StageActivate, RunID: uuid.New(), CorrelationID: "corr-2",
				PageCount: 3, CreatedAt: updatedAt, UpdatedAt: updatedAt,
			}},
		},
	}
	server := httptest.NewServer(newTestHandler(t, store))
	t.Cleanup(server.Close)
	client := newCookieClient(t)

	csrf := loginAPI(t, client, server.URL)
	if csrf == "" {
		t.Fatal("API login returned an empty CSRF token")
	}

	response := getAPI(t, client, server.URL+"/api/v1/documents?limit=10")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("list status=%d body=%s", response.StatusCode, body)
	}
	var list apiDocumentListResponse
	decodeResponse(t, response, &list)
	if len(list.Documents) != 1 || list.Documents[0].ID != documentID || list.Documents[0].ActiveVersion == nil ||
		*list.Documents[0].ActiveVersion != 2 {
		t.Fatalf("unexpected document projection: %#v", list)
	}

	response = getAPI(t, client, server.URL+"/api/v1/documents/"+documentID.String())
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("detail status=%d body=%s", response.StatusCode, body)
	}
	var detail apiDocumentResponse
	decodeResponse(t, response, &detail)
	if detail.ID != documentID || len(detail.Versions) != 1 || detail.Versions[0].ID != versionID ||
		detail.Versions[0].CorrelationID != "corr-2" {
		t.Fatalf("unexpected document detail: %#v", detail)
	}
}

func TestAPISessionRequiresCookieAndCSRFForMutation(t *testing.T) {
	server := httptest.NewServer(newTestHandler(t, &apiFixtureDocumentStore{}))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/api/v1/session")
	if err != nil {
		t.Fatalf("GET anonymous session: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("anonymous status=%d body=%s", response.StatusCode, body)
	}
	var anonymous apiError
	decodeResponse(t, response, &anonymous)
	if anonymous.Error.Code != "SESSION_EXPIRED" {
		t.Fatalf("anonymous error=%#v", anonymous)
	}

	client := newCookieClient(t)
	loginAPI(t, client, server.URL)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/processing-runs/"+uuid.NewString()+"/retry", bytes.NewBufferString(`{"requestKey":"retry-1"}`))
	if err != nil {
		t.Fatalf("new retry request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil {
		t.Fatalf("POST retry without CSRF: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("missing CSRF status=%d body=%s", response.StatusCode, body)
	}
	var rejected apiError
	decodeResponse(t, response, &rejected)
	if rejected.Error.Code != "CSRF_REJECTED" {
		t.Fatalf("missing CSRF error=%#v", rejected)
	}
}

func TestAPISourcePreservesExactDocumentVersionAndPage(t *testing.T) {
	documentID := uuid.New()
	versionID := uuid.New()
	store := &apiFixtureDocumentStore{source: documents.Source{Document: knowledge.SourceDocument{
		DocumentID: documentID, Name: "운영 정책", VersionID: versionID, Version: 3, PageCount: 7,
	}}}
	server := httptest.NewServer(newTestHandler(t, store))
	t.Cleanup(server.Close)
	client := newCookieClient(t)
	loginAPI(t, client, server.URL)

	response := getAPI(t, client, server.URL+"/api/v1/documents/"+documentID.String()+"/versions/3/source?page=5")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("source status=%d body=%s", response.StatusCode, body)
	}
	var source apiSourceResponse
	decodeResponse(t, response, &source)
	if source.DocumentID != documentID || source.VersionID != versionID || source.Version != 3 || source.Page != 5 ||
		source.SourceURL != "/sources/"+documentID.String()+"/versions/3?page=5" {
		t.Fatalf("source provenance changed: %#v", source)
	}
}

func TestAPIPreflightUsesThePublishedCamelCaseContract(t *testing.T) {
	store := &apiFixtureDocumentStore{preflight: documents.Preflight{
		FileName: "운영 정책.pdf", ByteSize: 1234, PageCount: 3,
		SHA256: "aabbcc", SuggestedName: "운영 정책",
	}}
	server := httptest.NewServer(newTestHandler(t, store))
	t.Cleanup(server.Close)
	client := newCookieClient(t)
	csrf := loginAPI(t, client, server.URL)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "운영 정책.pdf")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte("%PDF-test")); err != nil {
		t.Fatalf("write PDF: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/uploads/preflight", &body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("preflight status=%d body=%s", response.StatusCode, payload)
	}
	var got apiPreflightResponse
	decodeResponse(t, response, &got)
	if got.FileName != store.preflight.FileName || got.ByteSize != store.preflight.ByteSize ||
		got.PageCount != store.preflight.PageCount || got.SHA256 != store.preflight.SHA256 ||
		got.SuggestedName != store.preflight.SuggestedName {
		t.Fatalf("unexpected preflight response: %#v", got)
	}
}

func TestAPIRecoveryAcceptsTheRequestKeyAsAPathedResource(t *testing.T) {
	handler := newTestHandler(t, &apiFixtureDocumentStore{recovery: knowledge.UploadRecovery{State: knowledge.UploadNotCommitted}})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := newCookieClient(t)
	csrf := loginAPI(t, client, server.URL)

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/uploads/recovery/request-key", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("recovery status=%d body=%s", response.StatusCode, body)
	}
}

func TestAPISearchDoesNotExposeMCPTokenWhenUnavailable(t *testing.T) {
	server := httptest.NewServer(newTestHandler(t, &apiFixtureDocumentStore{}))
	t.Cleanup(server.Close)
	client := newCookieClient(t)
	loginAPI(t, client, server.URL)

	response := getAPI(t, client, server.URL+"/api/v1/search?q=연차")
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("search status=%d body=%s", response.StatusCode, body)
	}
	var unavailable apiError
	decodeResponse(t, response, &unavailable)
	if unavailable.Error.Code != "MCP_UNAVAILABLE" || !unavailable.Error.Retryable {
		t.Fatalf("search error=%#v", unavailable)
	}
}

func loginAPI(t *testing.T, client *http.Client, serverURL string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, serverURL+"/api/v1/session", bytes.NewBufferString(`{"username":"admin","password":"correct horse battery staple"}`))
	if err != nil {
		t.Fatalf("new API login request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("API login: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("login status=%d body=%s", response.StatusCode, body)
	}
	var session apiSessionResponse
	decodeResponse(t, response, &session)
	if session.User.Username != "admin" || session.User.Role != "DOCUMENT_ADMIN" || session.ExpiresAt.Before(time.Now()) {
		t.Fatalf("unexpected session response: %#v", session)
	}
	return session.CSRFToken
}

func newCookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{Jar: jar}
}

func getAPI(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new GET %s: %v", target, err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	return response
}

func decodeResponse(t *testing.T, response *http.Response, destination any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func pointer(value int) *int {
	return &value
}

type apiFixtureDocumentStore struct {
	list         []knowledge.DocumentSummary
	details      knowledge.DocumentDetails
	preflight    documents.Preflight
	registration knowledge.Registration
	recovery     knowledge.UploadRecovery
	source       documents.Source
	retryID      uuid.UUID
	readyErr     error
}

func (s *apiFixtureDocumentStore) ListDocuments(context.Context, int, int) ([]knowledge.DocumentSummary, error) {
	return s.list, nil
}

func (s *apiFixtureDocumentStore) GetDocument(context.Context, uuid.UUID) (knowledge.DocumentDetails, error) {
	if s.details.ID == uuid.Nil {
		return knowledge.DocumentDetails{}, knowledge.ErrNotFound
	}
	return s.details, nil
}

func (s *apiFixtureDocumentStore) Preflight(context.Context, string, []byte) (documents.Preflight, error) {
	return s.preflight, nil
}

func (s *apiFixtureDocumentStore) Register(context.Context, documents.RegisterCommand) (knowledge.Registration, error) {
	return s.registration, nil
}

func (s *apiFixtureDocumentStore) Source(context.Context, uuid.UUID, int) (documents.Source, error) {
	if s.source.Document.DocumentID == uuid.Nil {
		return documents.Source{}, knowledge.ErrNotFound
	}
	return s.source, nil
}

func (s *apiFixtureDocumentStore) RecoverRegistration(context.Context, string) (knowledge.UploadRecovery, error) {
	return s.recovery, nil
}

func (s *apiFixtureDocumentStore) Retry(context.Context, uuid.UUID, string) (uuid.UUID, error) {
	return s.retryID, nil
}

func (s *apiFixtureDocumentStore) Ready(context.Context) error {
	return s.readyErr
}
