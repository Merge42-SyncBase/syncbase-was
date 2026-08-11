package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/documents"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const (
	defaultDocumentLimit = 50
	maxDocumentLimit     = 100
	defaultSearchLimit   = 10
)

type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type apiLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type apiSessionResponse struct {
	User      apiUser   `json:"user"`
	CSRFToken string    `json:"csrfToken"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type apiUser struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

type apiDocumentListResponse struct {
	Documents []apiDocumentSummary `json:"documents"`
	Limit     int                  `json:"limit"`
	Offset    int                  `json:"offset"`
}

type apiDocumentSummary struct {
	ID            uuid.UUID               `json:"id"`
	Name          string                  `json:"name"`
	ActiveVersion *int                    `json:"activeVersion"`
	LatestVersion int                     `json:"latestVersion"`
	LatestStatus  knowledge.VersionStatus `json:"latestStatus"`
	UpdatedAt     time.Time               `json:"updatedAt"`
}

type apiDocumentResponse struct {
	ID            uuid.UUID    `json:"id"`
	Name          string       `json:"name"`
	ActiveVersion *int         `json:"activeVersion"`
	Versions      []apiVersion `json:"versions"`
	UpdatedAt     time.Time    `json:"updatedAt,omitempty"`
}

type apiVersion struct {
	ID                   uuid.UUID               `json:"id"`
	VersionNumber        int                     `json:"versionNumber"`
	Status               knowledge.VersionStatus `json:"status"`
	Active               bool                    `json:"active"`
	Stage                knowledge.Stage         `json:"stage"`
	RunID                uuid.UUID               `json:"runId"`
	ActivationOutcome    string                  `json:"activationOutcome"`
	ErrorCode            string                  `json:"errorCode,omitempty"`
	CorrelationID        string                  `json:"correlationId"`
	AutomaticAttempts    int                     `json:"automaticAttempts"`
	NextAutomaticRetryAt *time.Time              `json:"nextAutomaticRetryAt,omitempty"`
	ManualRetryAllowed   bool                    `json:"manualRetryAllowed"`
	QueuePosition        int                     `json:"queuePosition,omitempty"`
	PageCount            int                     `json:"pageCount"`
	CreatedAt            time.Time               `json:"createdAt"`
	UpdatedAt            time.Time               `json:"updatedAt"`
}

type apiRegistrationResponse struct {
	DocumentID      uuid.UUID               `json:"documentId"`
	VersionID       uuid.UUID               `json:"versionId"`
	Version         int                     `json:"version"`
	RunID           uuid.UUID               `json:"runId"`
	Status          knowledge.VersionStatus `json:"status"`
	Recovered       bool                    `json:"recovered"`
	DocumentURL     string                  `json:"documentUrl"`
	SourceViewerURL string                  `json:"sourceViewerUrl"`
}

type apiRecoveryResponse struct {
	Status       knowledge.UploadRecoveryState `json:"status"`
	Registration *apiRegistrationResponse      `json:"registration,omitempty"`
}

type apiRetryRequest struct {
	RequestKey string `json:"requestKey"`
}

type apiRetryResponse struct {
	RunID uuid.UUID `json:"runId"`
}

type apiSearchResponse struct {
	Query   string                `json:"query"`
	Results []knowledge.SearchHit `json:"results"`
}

type apiSourceResponse struct {
	DocumentID   uuid.UUID `json:"documentId"`
	DocumentName string    `json:"documentName"`
	VersionID    uuid.UUID `json:"versionId"`
	Version      int       `json:"version"`
	PageCount    int       `json:"pageCount"`
	Page         int       `json:"page"`
	SourceURL    string    `json:"sourceUrl"`
	RawPDFURL    string    `json:"rawPdfUrl"`
}

func (s *Server) apiLogin(response http.ResponseWriter, request *http.Request) {
	var input apiLoginRequest
	if err := decodeJSONBody(response, request, &input, maxLoginRequestBytes); err != nil {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "로그인 요청 형식을 확인하세요.", false)
		return
	}
	now := time.Now()
	loginKey := clientAddress(request.RemoteAddr)
	allowed, retryAfter := s.logins.allow(loginKey, now)
	if !allowed {
		response.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Round(time.Second).Seconds()))))
		writeAPIError(response, http.StatusTooManyRequests, "RATE_LIMITED", "로그인 시도가 잠시 제한되었습니다.", true)
		return
	}
	usernameMatches := subtleString(strings.TrimSpace(input.Username), s.config.AdminUsername) == 1
	passwordMatches := bcrypt.CompareHashAndPassword([]byte(s.config.AdminPasswordBcrypt), []byte(input.Password)) == nil
	if !usernameMatches || !passwordMatches {
		s.logins.recordFailure(loginKey, now)
		writeAPIError(response, http.StatusUnauthorized, "UNAUTHENTICATED", "아이디 또는 비밀번호가 올바르지 않습니다.", false)
		return
	}
	s.logins.reset(loginKey)
	current, err := s.issueSession(response)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, "INTERNAL", "로그인 세션을 만들지 못했습니다.", true)
		return
	}
	writeJSON(response, http.StatusOK, s.sessionResponse(current))
}

func (s *Server) apiSession(response http.ResponseWriter, request *http.Request) {
	current, ok := s.currentSession(request)
	if !ok {
		writeAPIError(response, http.StatusUnauthorized, "SESSION_EXPIRED", "로그인 세션이 만료되었습니다.", false)
		return
	}
	writeJSON(response, http.StatusOK, s.sessionResponse(current))
}

func (s *Server) apiLogout(response http.ResponseWriter, request *http.Request) {
	s.deleteSession(response, request)
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiListDocuments(response http.ResponseWriter, request *http.Request) {
	limit, err := queryInt(request, "limit", defaultDocumentLimit, 1, maxDocumentLimit)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "limit 값을 확인하세요.", false)
		return
	}
	offset, err := queryInt(request, "offset", 0, 0, int(^uint(0)>>1))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "offset 값을 확인하세요.", false)
		return
	}
	documents, err := s.documents.ListDocuments(request.Context(), limit, offset)
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	result := make([]apiDocumentSummary, len(documents))
	for index, document := range documents {
		result[index] = apiDocumentSummary{
			ID: document.ID, Name: document.Name, ActiveVersion: document.ActiveVersion,
			LatestVersion: document.LatestVersion, LatestStatus: document.LatestStatus,
			UpdatedAt: document.UpdatedAt,
		}
	}
	writeJSON(response, http.StatusOK, apiDocumentListResponse{Documents: result, Limit: limit, Offset: offset})
}

func (s *Server) apiDocument(response http.ResponseWriter, request *http.Request) {
	documentID, ok := apiPathUUID(response, request, "documentID")
	if !ok {
		return
	}
	document, err := s.documents.GetDocument(request.Context(), documentID)
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, documentResponse(document))
}

func (s *Server) apiPreflight(response http.ResponseWriter, request *http.Request) {
	content, name, err := s.readPDF(response, request)
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	result, err := s.documents.Preflight(request.Context(), name, content)
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiRegisterDocument(response http.ResponseWriter, request *http.Request) {
	s.apiRegister(response, request, knowledge.RegisterNewDocument, nil)
}

func (s *Server) apiRegisterVersion(response http.ResponseWriter, request *http.Request) {
	documentID, ok := apiPathUUID(response, request, "documentID")
	if !ok {
		return
	}
	s.apiRegister(response, request, knowledge.RegisterNewVersion, &documentID)
}

func (s *Server) apiRegister(
	response http.ResponseWriter,
	request *http.Request,
	operation knowledge.RegistrationOperation,
	target *uuid.UUID,
) {
	content, originalName, err := s.readPDF(response, request)
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	documentName := ""
	if operation == knowledge.RegisterNewDocument {
		documentName = request.FormValue("documentName")
	}
	registration, err := s.documents.Register(request.Context(), documents.RegisterCommand{
		RequestKey: request.FormValue("requestKey"), Operation: operation,
		TargetDocumentID: target, DocumentName: documentName,
		OriginalFileName: originalName, Content: content,
	})
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, registrationResponse(registration))
}

func (s *Server) apiRecovery(response http.ResponseWriter, request *http.Request) {
	recovery, err := s.documents.RecoverRegistration(request.Context(), request.URL.Query().Get("requestKey"))
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	result := apiRecoveryResponse{Status: recovery.State}
	if recovery.State == knowledge.UploadAccepted {
		registration := registrationResponse(recovery.Registration)
		result.Registration = &registration
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiRetryRun(response http.ResponseWriter, request *http.Request) {
	runID, err := uuid.Parse(request.PathValue("runID"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "처리 작업 ID를 확인하세요.", false)
		return
	}
	var input apiRetryRequest
	if err := decodeJSONBody(response, request, &input, maxLoginRequestBytes); err != nil {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "재시도 요청 형식을 확인하세요.", false)
		return
	}
	childRunID, err := s.documents.Retry(request.Context(), runID, input.RequestKey)
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, apiRetryResponse{RunID: childRunID})
}

func (s *Server) apiSearchDocuments(response http.ResponseWriter, request *http.Request) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	if query == "" {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "검색어를 입력하세요.", false)
		return
	}
	limit, err := queryInt(request, "limit", defaultSearchLimit, 1, 20)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "limit 값을 확인하세요.", false)
		return
	}
	if s.search == nil {
		writeAPIError(response, http.StatusServiceUnavailable, "MCP_UNAVAILABLE", "검색 연결이 설정되지 않았습니다.", true)
		return
	}
	hits, err := s.search.SearchDocuments(request.Context(), query, limit)
	if err != nil {
		s.writeAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, apiSearchResponse{Query: query, Results: hits})
}

func (s *Server) apiSource(response http.ResponseWriter, request *http.Request) {
	source, page, ok := s.apiLoadSource(response, request)
	if !ok {
		return
	}
	base := fmt.Sprintf("/sources/%s/versions/%d", source.Document.DocumentID, source.Document.Version)
	writeJSON(response, http.StatusOK, apiSourceResponse{
		DocumentID: source.Document.DocumentID, DocumentName: source.Document.Name,
		VersionID: source.Document.VersionID, Version: source.Document.Version,
		PageCount: source.Document.PageCount, Page: page,
		SourceURL: base + "?page=" + strconv.Itoa(page),
		RawPDFURL: fmt.Sprintf("/api/v1/documents/%s/versions/%d/raw.pdf", source.Document.DocumentID, source.Document.Version),
	})
}

func (s *Server) apiSourceRaw(response http.ResponseWriter, request *http.Request) {
	source, _, ok := s.apiLoadSource(response, request)
	if !ok {
		return
	}
	response.Header().Set("Content-Type", "application/pdf")
	response.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=syncbase-v%d.pdf", source.Document.Version))
	http.ServeFile(response, request, source.Path)
}

func (s *Server) apiLoadSource(
	response http.ResponseWriter,
	request *http.Request,
) (documents.Source, int, bool) {
	documentID, ok := apiPathUUID(response, request, "documentID")
	if !ok {
		return documents.Source{}, 0, false
	}
	version, err := strconv.Atoi(request.PathValue("version"))
	if err != nil || version < 1 {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "버전을 확인하세요.", false)
		return documents.Source{}, 0, false
	}
	source, err := s.documents.Source(request.Context(), documentID, version)
	if err != nil {
		s.writeAPIError(response, err)
		return documents.Source{}, 0, false
	}
	page, err := queryInt(request, "page", 1, 1, max(1, source.Document.PageCount))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "페이지를 확인하세요.", false)
		return documents.Source{}, 0, false
	}
	return source, page, true
}

func (s *Server) sessionResponse(current session) apiSessionResponse {
	return apiSessionResponse{
		User:      apiUser{Username: s.config.AdminUsername, Role: "DOCUMENT_ADMIN"},
		CSRFToken: current.csrf,
		ExpiresAt: current.expiresAt,
	}
}

func documentResponse(document knowledge.DocumentDetails) apiDocumentResponse {
	versions := make([]apiVersion, len(document.Versions))
	updatedAt := time.Time{}
	for index, version := range document.Versions {
		versions[index] = apiVersion{
			ID: version.ID, VersionNumber: version.VersionNumber, Status: version.Status,
			Active: version.Active, Stage: version.Stage, RunID: version.RunID,
			ActivationOutcome: version.ActivationOutcome, ErrorCode: version.ErrorCode,
			CorrelationID: version.CorrelationID, AutomaticAttempts: version.AutomaticAttempts,
			NextAutomaticRetryAt: version.NextAutomaticRetryAt, ManualRetryAllowed: version.ManualRetryAllowed,
			QueuePosition: version.QueuePosition, PageCount: version.PageCount,
			CreatedAt: version.CreatedAt, UpdatedAt: version.UpdatedAt,
		}
		if version.UpdatedAt.After(updatedAt) {
			updatedAt = version.UpdatedAt
		}
	}
	return apiDocumentResponse{
		ID: document.ID, Name: document.Name, ActiveVersion: document.ActiveVersion,
		Versions: versions, UpdatedAt: updatedAt,
	}
}

func registrationResponse(registration knowledge.Registration) apiRegistrationResponse {
	documentURL := "/documents/" + registration.DocumentID.String()
	return apiRegistrationResponse{
		DocumentID: registration.DocumentID, VersionID: registration.VersionID,
		Version: registration.Version, RunID: registration.RunID, Status: registration.Status,
		Recovered: registration.Recovered, DocumentURL: documentURL,
		SourceViewerURL: fmt.Sprintf("/sources/%s/versions/%d?page=1", registration.DocumentID, registration.Version),
	}
}

func (s *Server) issueSession(response http.ResponseWriter) (session, error) {
	token, err := randomToken()
	if err != nil {
		return session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return session{}, err
	}
	current := session{csrf: csrf, expiresAt: time.Now().Add(sessionTTL)}
	s.sessionMu.Lock()
	s.sessions[token] = current
	s.sessionMu.Unlock()
	http.SetCookie(response, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.config.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	return current, nil
}

func (s *Server) deleteSession(response http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(sessionCookie); err == nil {
		s.sessionMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionMu.Unlock()
	}
	http.SetCookie(response, &http.Cookie{
		Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: s.config.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) writeAPIError(response http.ResponseWriter, err error) {
	status, code, message, retryable := apiErrorFor(err)
	writeAPIError(response, status, code, message, retryable)
}

func apiErrorFor(err error) (int, string, string, bool) {
	switch {
	case errors.Is(err, knowledge.ErrInvalidArgument):
		return http.StatusBadRequest, "INVALID_ARGUMENT", "입력값을 확인하세요.", false
	case errors.Is(err, knowledge.ErrInvalidPDF):
		return http.StatusBadRequest, "INVALID_PDF", "PDF 파일을 확인하세요.", false
	case errors.Is(err, knowledge.ErrNotFound):
		return http.StatusNotFound, "NOT_FOUND", "요청한 문서를 찾을 수 없습니다.", false
	case errors.Is(err, knowledge.ErrIdempotencyConflict):
		return http.StatusConflict, "IDEMPOTENCY_CONFLICT", "복구 코드가 다른 등록 요청과 충돌합니다.", false
	case errors.Is(err, knowledge.ErrProfileMismatch):
		return http.StatusConflict, "PROFILE_MISMATCH", "임베딩 프로필이 일치하지 않아 요청을 중지했습니다.", false
	case errors.Is(err, knowledge.ErrUnauthenticated):
		return http.StatusBadGateway, "MCP_UNAUTHENTICATED", "검색 연결 인증을 확인하세요.", false
	case errors.Is(err, knowledge.ErrQueueFull):
		return http.StatusServiceUnavailable, "QUEUE_FULL", "처리 대기열이 가득 찼습니다. 잠시 후 다시 시도하세요.", true
	case errors.Is(err, knowledge.ErrTemporarilyUnavailable), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "TEMPORARILY_UNAVAILABLE", "잠시 후 다시 시도하세요.", true
	default:
		return http.StatusInternalServerError, "INTERNAL", "요청을 처리하지 못했습니다.", true
	}
}

func writeAPIError(response http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(response, status, apiError{Error: apiErrorBody{Code: code, Message: message, Retryable: retryable}})
}

func apiPathUUID(response http.ResponseWriter, request *http.Request, name string) (uuid.UUID, bool) {
	value, err := uuid.Parse(request.PathValue(name))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, "INVALID_ARGUMENT", "문서 ID를 확인하세요.", false)
		return uuid.Nil, false
	}
	return value, true
}

func queryInt(request *http.Request, name string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(request.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, knowledge.ErrInvalidArgument
	}
	return value, nil
}

func decodeJSONBody(response http.ResponseWriter, request *http.Request, destination any, limit int64) error {
	request.Body = http.MaxBytesReader(response, request.Body, limit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}
