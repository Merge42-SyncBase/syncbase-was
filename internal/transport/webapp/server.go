// Package webapp serves the single-administrator SyncBase console.
package webapp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	frontend "github.com/Merge42-SyncBase/syncbase-frontend"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/documents"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie          = "syncbase_session"
	sessionTTL             = 30 * time.Minute
	loginFailureLimit      = 5
	loginFailureWindow     = 5 * time.Minute
	maxLoginFailureEntries = 1024
	maxLoginRequestBytes   = 16 << 10
)

// Config defines the administrator console's runtime settings.
type Config struct {
	AdminUsername       string
	AdminPasswordBcrypt string
	CookieSecure        bool
	StaticRoot          string
	EnvironmentLabel    string
	MCPURL              string
	MCPToken            string
}

type documentService interface {
	ListDocuments(context.Context, int, int) ([]knowledge.DocumentSummary, error)
	GetDocument(context.Context, uuid.UUID) (knowledge.DocumentDetails, error)
	Preflight(context.Context, string, []byte) (documents.Preflight, error)
	Register(context.Context, documents.RegisterCommand) (knowledge.Registration, error)
	Source(context.Context, uuid.UUID, int) (documents.Source, error)
	RecoverRegistration(context.Context, string) (knowledge.UploadRecovery, error)
	Retry(context.Context, uuid.UUID, string) (uuid.UUID, error)
	Ready(context.Context) error
}

type session struct {
	csrf      string
	expiresAt time.Time
}

type loginFailure struct {
	count    int
	resetAt  time.Time
	lastSeen time.Time
}

type loginGuard struct {
	mu       sync.Mutex
	failures map[string]loginFailure
}

// Server owns the web console's handlers and administrator sessions.
type Server struct {
	config    Config
	documents documentService
	templates *template.Template
	sessions  map[string]session
	sessionMu sync.Mutex
	logins    loginGuard
	search    *mcpClient
}

// New returns the configured administrator console HTTP handler.
func New(config Config, documents documentService) (http.Handler, error) {
	cost, passwordErr := bcrypt.Cost([]byte(config.AdminPasswordBcrypt))
	if strings.TrimSpace(config.AdminUsername) == "" || passwordErr != nil ||
		cost < bcrypt.MinCost || documents == nil {
		return nil, fmt.Errorf("invalid web configuration: %w", knowledge.ErrInvalidArgument)
	}
	templates, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}
	var search *mcpClient
	if config.MCPURL != "" || config.MCPToken != "" {
		search, err = newMCPClient(config.MCPURL, config.MCPToken, &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("configure MCP search client: %w", err)
		}
	}
	server := &Server{
		config:    config,
		documents: documents,
		templates: templates,
		sessions:  make(map[string]session),
		logins:    loginGuard{failures: make(map[string]loginFailure)},
		search:    search,
	}
	mux := http.NewServeMux()
	static := http.Handler(nil)
	if strings.TrimSpace(config.StaticRoot) == "" {
		files, err := frontend.Static()
		if err != nil {
			return nil, fmt.Errorf("open embedded frontend: %w", err)
		}
		static = http.FileServer(http.FS(files))
	} else {
		static = http.FileServer(http.Dir(config.StaticRoot))
	}
	mux.Handle("GET /css/", static)
	mux.Handle("GET /js/", static)
	mux.Handle("GET /vendor/", static)
	mux.Handle("GET /favicon.svg", static)
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"ok","runtime":"go"}`))
	})
	mux.HandleFunc("GET /readyz", server.readiness)
	mux.HandleFunc("GET /login", server.loginPage)
	mux.HandleFunc("POST /login", server.login)
	mux.Handle("GET /{$}", server.auth(http.HandlerFunc(server.home)))
	mux.Handle("POST /logout", server.auth(server.csrf(http.HandlerFunc(server.logout))))
	mux.Handle("GET /documents", server.auth(http.HandlerFunc(server.documentList)))
	mux.Handle("GET /search", server.auth(http.HandlerFunc(server.searchDocuments)))
	mux.Handle("GET /documents/new", server.auth(http.HandlerFunc(server.newDocument)))
	mux.Handle("POST /api/uploads/preflight", server.auth(server.csrf(http.HandlerFunc(server.preflight))))
	mux.Handle("GET /api/uploads/recovery", server.auth(http.HandlerFunc(server.recovery)))
	mux.Handle("POST /documents", server.auth(server.csrf(http.HandlerFunc(server.registerDocument))))
	mux.Handle("GET /documents/{documentID}", server.auth(http.HandlerFunc(server.documentDetails)))
	mux.Handle("GET /documents/{documentID}/versions/new", server.auth(http.HandlerFunc(server.newVersion)))
	mux.Handle("POST /documents/{documentID}/versions", server.auth(server.csrf(http.HandlerFunc(server.registerVersion))))
	mux.Handle("POST /processing-runs/{runID}/retry", server.auth(server.csrf(http.HandlerFunc(server.retryRun))))
	mux.Handle("GET /sources/{documentID}/versions/{version}", server.auth(http.HandlerFunc(server.sourceViewer)))
	mux.Handle("GET /sources/{documentID}/versions/{version}/raw.pdf", server.auth(http.HandlerFunc(server.sourceRaw)))

	// The React console is served by a separate same-origin Web service. Keep
	// its contract under /api/v1 so the UI never needs direct access to the
	// database, object store, embedding runtime, or MCP credential.
	mux.Handle("POST /api/v1/session", http.HandlerFunc(server.apiLogin))
	mux.Handle("GET /api/v1/session", server.auth(http.HandlerFunc(server.apiSession)))
	mux.Handle("DELETE /api/v1/session", server.auth(server.csrf(http.HandlerFunc(server.apiLogout))))
	mux.Handle("GET /api/v1/documents", server.auth(http.HandlerFunc(server.apiListDocuments)))
	mux.Handle("GET /api/v1/documents/{documentID}", server.auth(http.HandlerFunc(server.apiDocument)))
	mux.Handle("POST /api/v1/documents", server.auth(server.csrf(http.HandlerFunc(server.apiRegisterDocument))))
	mux.Handle("POST /api/v1/documents/{documentID}/versions", server.auth(server.csrf(http.HandlerFunc(server.apiRegisterVersion))))
	mux.Handle("POST /api/v1/uploads/preflight", server.auth(server.csrf(http.HandlerFunc(server.apiPreflight))))
	mux.Handle("GET /api/v1/uploads/recovery", server.auth(http.HandlerFunc(server.apiRecovery)))
	mux.Handle("POST /api/v1/processing-runs/{runID}/retry", server.auth(server.csrf(http.HandlerFunc(server.apiRetryRun))))
	mux.Handle("GET /api/v1/search", server.auth(http.HandlerFunc(server.apiSearchDocuments)))
	mux.Handle("GET /api/v1/documents/{documentID}/versions/{version}/source", server.auth(http.HandlerFunc(server.apiSource)))
	mux.Handle("GET /api/v1/documents/{documentID}/versions/{version}/raw.pdf", server.auth(http.HandlerFunc(server.apiSourceRaw)))
	return securityHeaders(mux), nil
}

func (s *Server) readiness(response http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := s.documents.Ready(ctx); err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	if s.search != nil {
		if err := s.search.Ready(ctx); err != nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) searchDocuments(response http.ResponseWriter, request *http.Request) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	var hits []knowledge.SearchHit
	message := ""
	if query != "" {
		if s.search == nil {
			message = "MCP 검색 연결이 설정되지 않았습니다."
		} else {
			var err error
			hits, err = s.search.SearchDocuments(request.Context(), query, 10)
			if err != nil {
				switch {
				case errors.Is(err, knowledge.ErrProfileMismatch):
					message = "처리 프로필이 일치하지 않아 검색을 중지했습니다."
				case errors.Is(err, knowledge.ErrUnauthenticated):
					message = "MCP 인증을 확인해 주세요."
				default:
					message = "MCP 검색이 잠시 지연되고 있습니다. 다시 시도해 주세요."
				}
			}
		}
	}
	s.render(response, "search", map[string]any{
		"Query": query, "Hits": hits, "Error": message,
		"Environment": s.config.EnvironmentLabel,
	})
}

func (s *Server) loginPage(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.currentSession(request); ok {
		http.Redirect(response, request, "/documents", http.StatusSeeOther)
		return
	}
	s.render(response, "login", map[string]any{
		"Error":    request.URL.Query().Has("error"),
		"Next":     safeNext(request.URL.Query().Get("next")),
		"Username": "",
	})
}

func (s *Server) login(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, maxLoginRequestBytes)
	if err := request.ParseForm(); err != nil {
		http.Error(response, "잘못된 로그인 요청입니다.", http.StatusBadRequest)
		return
	}
	now := time.Now()
	loginKey := clientAddress(request.RemoteAddr)
	allowed, retryAfter := s.logins.allow(loginKey, now)
	if !allowed {
		response.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Round(time.Second).Seconds()))))
		s.renderStatus(response, http.StatusTooManyRequests, "login", map[string]any{
			"Error": true, "Next": safeNext(request.FormValue("next")), "Username": request.FormValue("username"),
		})
		return
	}
	username := request.FormValue("username")
	password := request.FormValue("password")
	usernameMatches := subtleString(username, s.config.AdminUsername) == 1
	passwordMatches := bcrypt.CompareHashAndPassword([]byte(s.config.AdminPasswordBcrypt), []byte(password)) == nil
	if !usernameMatches || !passwordMatches {
		s.logins.recordFailure(loginKey, now)
		s.renderStatus(response, http.StatusUnauthorized, "login", map[string]any{
			"Error": true, "Next": safeNext(request.FormValue("next")), "Username": username,
		})
		return
	}
	s.logins.reset(loginKey)
	if _, err := s.issueSession(response); err != nil {
		http.Error(response, "로그인 세션을 만들지 못했습니다.", http.StatusInternalServerError)
		return
	}
	destination := safeNext(request.FormValue("next"))
	if destination == "" {
		destination = "/documents"
	}
	http.Redirect(response, request, destination, http.StatusSeeOther)
}

func (s *Server) logout(response http.ResponseWriter, request *http.Request) {
	s.deleteSession(response, request)
	http.Redirect(response, request, "/login", http.StatusSeeOther)
}

func (s *Server) home(response http.ResponseWriter, request *http.Request) {
	http.Redirect(response, request, "/documents", http.StatusSeeOther)
}

func (s *Server) documentList(response http.ResponseWriter, request *http.Request) {
	documents, err := s.documents.ListDocuments(request.Context(), 50, 0)
	if err != nil {
		s.webError(response, err)
		return
	}
	metrics := struct{ searchable, processing, attention int }{}
	for _, document := range documents {
		if document.ActiveVersion != nil {
			metrics.searchable++
		}
		switch document.LatestStatus {
		case knowledge.VersionQueued, knowledge.VersionProcessing:
			metrics.processing++
		case knowledge.VersionFailed:
			metrics.attention++
		}
	}
	s.render(response, "documents", map[string]any{
		"Documents":   documents,
		"Total":       len(documents),
		"Searchable":  metrics.searchable,
		"Processing":  metrics.processing,
		"Attention":   metrics.attention,
		"CSRF":        s.mustSession(request).csrf,
		"Environment": s.config.EnvironmentLabel,
	})
}

func (s *Server) newDocument(response http.ResponseWriter, request *http.Request) {
	s.renderUpload(response, request, nil)
}

func (s *Server) newVersion(response http.ResponseWriter, request *http.Request) {
	documentID, ok := pathUUID(response, request, "documentID")
	if !ok {
		return
	}
	document, err := s.documents.GetDocument(request.Context(), documentID)
	if err != nil {
		s.webError(response, err)
		return
	}
	s.renderUpload(response, request, &document)
}

func (s *Server) renderUpload(response http.ResponseWriter, request *http.Request, document *knowledge.DocumentDetails) {
	action := "/documents"
	if document != nil {
		action = "/documents/" + document.ID.String() + "/versions"
	}
	s.render(response, "upload", map[string]any{
		"Document": document,
		"Action":   action,
		"CSRF":     s.mustSession(request).csrf,
	})
}

func (s *Server) preflight(response http.ResponseWriter, request *http.Request) {
	content, name, err := s.readPDF(response, request)
	if err != nil {
		s.webError(response, err)
		return
	}
	result, err := s.documents.Preflight(request.Context(), name, content)
	if err != nil {
		s.webError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"fileName": result.FileName, "byteSize": result.ByteSize, "pageCount": result.PageCount,
		"sha256": result.SHA256, "suggestedName": result.SuggestedName,
	})
}

func (s *Server) recovery(response http.ResponseWriter, request *http.Request) {
	recovery, err := s.documents.RecoverRegistration(request.Context(), request.URL.Query().Get("requestKey"))
	if err != nil {
		s.webError(response, err)
		return
	}
	if recovery.State != knowledge.UploadAccepted {
		writeJSON(response, http.StatusOK, map[string]any{"status": recovery.State})
		return
	}
	registration := recovery.Registration
	writeJSON(response, http.StatusOK, map[string]any{
		"status": "accepted", "documentId": registration.DocumentID,
		"versionNumber": registration.Version, "processingRunId": registration.RunID,
		"processingStatus": registration.Status, "documentUrl": "/documents/" + registration.DocumentID.String(),
	})
}

func (s *Server) registerDocument(response http.ResponseWriter, request *http.Request) {
	s.register(response, request, knowledge.RegisterNewDocument, nil)
}

func (s *Server) registerVersion(response http.ResponseWriter, request *http.Request) {
	documentID, ok := pathUUID(response, request, "documentID")
	if !ok {
		return
	}
	s.register(response, request, knowledge.RegisterNewVersion, &documentID)
}

func (s *Server) register(
	response http.ResponseWriter,
	request *http.Request,
	operation knowledge.RegistrationOperation,
	target *uuid.UUID,
) {
	content, originalName, err := s.readPDF(response, request)
	if err != nil {
		s.webError(response, err)
		return
	}
	documentName := ""
	if operation == knowledge.RegisterNewDocument {
		documentName = request.FormValue("documentName")
	}
	result, err := s.documents.Register(request.Context(), documents.RegisterCommand{
		RequestKey: request.FormValue("requestKey"), Operation: operation,
		TargetDocumentID: target, DocumentName: documentName,
		OriginalFileName: originalName, Content: content,
	})
	if err != nil {
		s.webError(response, err)
		return
	}
	http.Redirect(response, request, "/documents/"+result.DocumentID.String()+"?registered=1", http.StatusSeeOther)
}

func (s *Server) documentDetails(response http.ResponseWriter, request *http.Request) {
	documentID, ok := pathUUID(response, request, "documentID")
	if !ok {
		return
	}
	document, err := s.documents.GetDocument(request.Context(), documentID)
	if err != nil {
		s.webError(response, err)
		return
	}
	processing := false
	for _, version := range document.Versions {
		if version.Status == knowledge.VersionQueued || version.Status == knowledge.VersionProcessing {
			processing = true
		}
	}
	s.render(response, "details", map[string]any{
		"Document": document, "Processing": processing,
		"Registered": request.URL.Query().Has("registered"),
		"CSRF":       s.mustSession(request).csrf,
	})
}

func (s *Server) retryRun(response http.ResponseWriter, request *http.Request) {
	runID, err := uuid.Parse(request.PathValue("runID"))
	if err != nil {
		http.Error(response, "잘못된 처리 작업 ID입니다.", http.StatusBadRequest)
		return
	}
	documentID, err := uuid.Parse(request.FormValue("documentId"))
	if err != nil {
		http.Error(response, "잘못된 문서 ID입니다.", http.StatusBadRequest)
		return
	}
	if _, err := s.documents.Retry(request.Context(), runID, request.FormValue("requestKey")); err != nil {
		s.webError(response, err)
		return
	}
	http.Redirect(response, request, "/documents/"+documentID.String()+"?retried=1", http.StatusSeeOther)
}

func (s *Server) sourceViewer(response http.ResponseWriter, request *http.Request) {
	source, page, ok := s.loadSource(response, request)
	if !ok {
		return
	}
	s.render(response, "source", map[string]any{
		"Source": source.Document, "Page": page,
		"SourceURL": fmt.Sprintf(
			"/sources/%s/versions/%d?page=%d",
			source.Document.DocumentID,
			source.Document.Version,
			page,
		),
	})
}

func (s *Server) sourceRaw(response http.ResponseWriter, request *http.Request) {
	source, _, ok := s.loadSource(response, request)
	if !ok {
		return
	}
	response.Header().Set("Content-Type", "application/pdf")
	response.Header().Set(
		"Content-Disposition",
		fmt.Sprintf("inline; filename=syncbase-v%d.pdf", source.Document.Version),
	)
	http.ServeFile(response, request, source.Path)
}

func (s *Server) loadSource(
	response http.ResponseWriter,
	request *http.Request,
) (documents.Source, int, bool) {
	documentID, ok := pathUUID(response, request, "documentID")
	if !ok {
		return documents.Source{}, 0, false
	}
	version, err := strconv.Atoi(request.PathValue("version"))
	if err != nil || version < 1 {
		http.Error(response, "잘못된 버전입니다.", http.StatusBadRequest)
		return documents.Source{}, 0, false
	}
	source, err := s.documents.Source(request.Context(), documentID, version)
	if err != nil {
		s.webError(response, err)
		return documents.Source{}, 0, false
	}
	page := 1
	if raw := request.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
	}
	if err != nil || page < 1 ||
		(source.Document.PageCount > 0 && page > source.Document.PageCount) {
		http.Error(response, "잘못된 페이지입니다.", http.StatusBadRequest)
		return documents.Source{}, 0, false
	}
	return source, page, true
}

func (s *Server) readPDF(response http.ResponseWriter, request *http.Request) ([]byte, string, error) {
	request.Body = http.MaxBytesReader(response, request.Body, knowledge.MaxUploadBytes+1024*1024)
	if err := request.ParseMultipartForm(knowledge.MaxUploadBytes); err != nil {
		return nil, "", fmt.Errorf("parse PDF upload: %w", knowledge.ErrInvalidPDF)
	}
	file, header, err := request.FormFile("file")
	if err != nil {
		return nil, "", fmt.Errorf("read PDF upload: %w", knowledge.ErrInvalidPDF)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, knowledge.MaxUploadBytes+1))
	if err != nil || len(content) < 5 || len(content) > knowledge.MaxUploadBytes || string(content[:5]) != "%PDF-" {
		return nil, "", fmt.Errorf("invalid PDF upload: %w", knowledge.ErrInvalidPDF)
	}
	name := filepath.Base(header.Filename)
	if name == "." || name == "" || len([]rune(name)) > 255 {
		return nil, "", fmt.Errorf("invalid PDF filename: %w", knowledge.ErrInvalidArgument)
	}
	return content, name, nil
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if _, ok := s.currentSession(request); !ok {
			nextPath := request.URL.RequestURI()
			if candidate := safeNext(request.Header.Get("X-SyncBase-Return-To")); candidate != "" {
				nextPath = candidate
			}
			if strings.HasPrefix(request.URL.Path, "/api/v1/") {
				writeAPIError(response, http.StatusUnauthorized, "SESSION_EXPIRED", "로그인 세션이 만료되었습니다.", false)
				return
			}
			if strings.Contains(request.Header.Get("Accept"), "application/json") ||
				strings.HasPrefix(request.URL.Path, "/api/") {
				writeJSON(response, http.StatusUnauthorized, map[string]any{
					"status":   "session_expired",
					"loginUrl": "/login?next=" + url.QueryEscape(nextPath),
				})
				return
			}
			http.Redirect(response, request, "/login?next="+url.QueryEscape(nextPath), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		current, ok := s.currentSession(request)
		if !ok {
			if isAPIRequest(request) {
				writeAPIError(response, http.StatusUnauthorized, "SESSION_EXPIRED", "로그인 세션이 만료되었습니다.", false)
				return
			}
			http.Error(response, "세션이 만료되었습니다.", http.StatusUnauthorized)
			return
		}
		provided := request.Header.Get("X-CSRF-Token")
		if provided == "" {
			if strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data") {
				request.Body = http.MaxBytesReader(response, request.Body, knowledge.MaxUploadBytes+1024*1024)
			}
			provided = request.FormValue("csrf")
		}
		if subtleString(provided, current.csrf) == 0 {
			if isAPIRequest(request) {
				writeAPIError(response, http.StatusForbidden, "CSRF_REJECTED", "요청 검증에 실패했습니다.", false)
				return
			}
			http.Error(response, "요청 검증에 실패했습니다.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) currentSession(request *http.Request) (session, bool) {
	cookie, err := request.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	now := time.Now()
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	current, found := s.sessions[cookie.Value]
	if !found || now.After(current.expiresAt) {
		delete(s.sessions, cookie.Value)
		return session{}, false
	}
	current.expiresAt = now.Add(sessionTTL)
	s.sessions[cookie.Value] = current
	return current, true
}

func (s *Server) mustSession(request *http.Request) session {
	current, _ := s.currentSession(request)
	return current
}

func (s *Server) render(response http.ResponseWriter, name string, data any) {
	s.renderStatus(response, http.StatusOK, name, data)
}

func (s *Server) renderStatus(response http.ResponseWriter, status int, name string, data any) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(status)
	if err := s.templates.ExecuteTemplate(response, name, data); err != nil {
		slog.Error("render web template", "template", name, "error", err)
	}
}

func (s *Server) webError(response http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "요청을 처리하지 못했습니다."
	switch {
	case errors.Is(err, knowledge.ErrInvalidArgument), errors.Is(err, knowledge.ErrInvalidPDF):
		status, message = http.StatusBadRequest, "입력값 또는 PDF를 확인하세요."
	case errors.Is(err, knowledge.ErrNotFound):
		status, message = http.StatusNotFound, "요청한 문서를 찾을 수 없습니다."
	case errors.Is(err, knowledge.ErrQueueFull), errors.Is(err, knowledge.ErrTemporarilyUnavailable):
		status, message = http.StatusServiceUnavailable, "잠시 후 다시 시도하세요."
	case errors.Is(err, knowledge.ErrIdempotencyConflict):
		status, message = http.StatusConflict, "등록 복구 코드가 다른 요청과 충돌합니다."
	}
	http.Error(response, message, status)
}

func isAPIRequest(request *http.Request) bool {
	return strings.HasPrefix(request.URL.Path, "/api/") ||
		strings.Contains(request.Header.Get("Accept"), "application/json")
}

func pathUUID(response http.ResponseWriter, request *http.Request, name string) (uuid.UUID, bool) {
	value, err := uuid.Parse(request.PathValue(name))
	if err != nil {
		http.Error(response, "잘못된 문서 ID입니다.", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return value, true
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func randomToken() (string, error) {
	content := make([]byte, 32)
	if _, err := rand.Read(content); err != nil {
		return "", err
	}
	return hex.EncodeToString(content), nil
}

func subtleString(left, right string) int {
	leftDigest := sha256.Sum256([]byte(left))
	rightDigest := sha256.Sum256([]byte(right))
	if len(left) != len(right) {
		return 0
	}
	return subtle.ConstantTimeCompare(leftDigest[:], rightDigest[:])
}

func safeNext(value string) string {
	if value == "" || strings.HasPrefix(value, "//") {
		return ""
	}
	for _, character := range value {
		if character == '\\' || unicode.IsControl(character) {
			return ""
		}
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Path == "" ||
		!strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") ||
		strings.Contains(parsed.Path, "\\") {
		return ""
	}
	return parsed.RequestURI()
}

func clientAddress(remoteAddress string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err == nil && host != "" {
		return host
	}
	if strings.TrimSpace(remoteAddress) == "" {
		return "unknown"
	}
	return remoteAddress
}

func (g *loginGuard) allow(key string, now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	failure, found := g.failures[key]
	if !found {
		return true, 0
	}
	if !now.Before(failure.resetAt) {
		delete(g.failures, key)
		return true, 0
	}
	if failure.count < loginFailureLimit {
		return true, 0
	}
	return false, failure.resetAt.Sub(now)
}

func (g *loginGuard) recordFailure(key string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	failure := g.failures[key]
	if !now.Before(failure.resetAt) {
		failure = loginFailure{resetAt: now.Add(loginFailureWindow)}
	}
	failure.count++
	failure.lastSeen = now
	g.failures[key] = failure
	if len(g.failures) <= maxLoginFailureEntries {
		return
	}
	oldestKey := ""
	oldest := now
	for candidate, entry := range g.failures {
		if !now.Before(entry.resetAt) {
			delete(g.failures, candidate)
			continue
		}
		if oldestKey == "" || entry.lastSeen.Before(oldest) {
			oldestKey = candidate
			oldest = entry.lastSeen
		}
	}
	if len(g.failures) > maxLoginFailureEntries && oldestKey != "" {
		delete(g.failures, oldestKey)
	}
}

func (g *loginGuard) reset(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.failures, key)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "SAMEORIGIN")
		response.Header().Set("Referrer-Policy", "same-origin")
		response.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-src 'self'; object-src 'self'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(response, request)
	})
}
