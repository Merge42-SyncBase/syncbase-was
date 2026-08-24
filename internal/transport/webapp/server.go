// Package webapp serves SyncBase's authenticated browser API.
package webapp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/documents"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/sessions"
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

// Config defines the browser API's runtime settings.
type Config struct {
	AdminUsername       string
	AdminPasswordBcrypt string
	CookieSecure        bool
	MCPURL              string
	MCPToken            string
	WorkerReadyURL      string
	Sessions            sessions.Store
}

type documentService interface {
	ListDocuments(context.Context, int, int) ([]knowledge.DocumentSummary, error)
	FindNameMatches(context.Context, string, int) (documents.NameMatches, error)
	GetDocument(context.Context, uuid.UUID) (knowledge.DocumentDetails, error)
	Preflight(context.Context, string, []byte) (documents.Preflight, error)
	Register(context.Context, documents.RegisterCommand) (knowledge.Registration, error)
	Source(context.Context, uuid.UUID, int) (documents.Source, error)
	RecoverRegistration(context.Context, string) (knowledge.UploadRecovery, error)
	Retry(context.Context, uuid.UUID, string) (uuid.UUID, error)
	Ready(context.Context) error
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

// Server owns API handlers, sessions, and the server-side MCP client.
type Server struct {
	config    Config
	documents documentService
	sessions  sessions.Store
	logins    loginGuard
	search    *mcpClient
	worker    *readinessClient
}

// New returns the configured browser API handler.
func New(config Config, documents documentService) (http.Handler, error) {
	cost, passwordErr := bcrypt.Cost([]byte(config.AdminPasswordBcrypt))
	if strings.TrimSpace(config.AdminUsername) == "" || passwordErr != nil ||
		cost < bcrypt.MinCost || documents == nil || config.Sessions == nil {
		return nil, fmt.Errorf("invalid web configuration: %w", knowledge.ErrInvalidArgument)
	}
	var search *mcpClient
	if config.MCPURL != "" || config.MCPToken != "" {
		var err error
		search, err = newMCPClient(config.MCPURL, config.MCPToken, &http.Client{Timeout: 15 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("configure MCP search client: %w", err)
		}
	}
	var worker *readinessClient
	if config.WorkerReadyURL != "" {
		var err error
		worker, err = newReadinessClient(config.WorkerReadyURL, &http.Client{Timeout: 3 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("configure worker readiness client: %w", err)
		}
	}
	server := &Server{
		config: config, documents: documents, sessions: config.Sessions,
		logins: loginGuard{failures: make(map[string]loginFailure)}, search: search, worker: worker,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok", "runtime": "go-api"})
	})
	mux.HandleFunc("GET /readyz", server.readiness)
	mux.Handle("POST /api/v1/session", http.HandlerFunc(server.apiLogin))
	mux.Handle("GET /api/v1/session", server.auth(http.HandlerFunc(server.apiSession)))
	mux.Handle("DELETE /api/v1/session", server.auth(server.csrf(http.HandlerFunc(server.apiLogout))))
	mux.Handle("GET /api/v1/documents", server.auth(http.HandlerFunc(server.apiListDocuments)))
	mux.Handle("GET /api/v1/documents/name-matches", server.auth(http.HandlerFunc(server.apiDocumentNameMatches)))
	mux.Handle("GET /api/v1/documents/{documentID}", server.auth(http.HandlerFunc(server.apiDocument)))
	mux.Handle("POST /api/v1/documents", server.auth(server.csrf(http.HandlerFunc(server.apiRegisterDocument))))
	mux.Handle("POST /api/v1/documents/{documentID}/versions", server.auth(server.csrf(http.HandlerFunc(server.apiRegisterVersion))))
	mux.Handle("POST /api/v1/uploads/preflight", server.auth(server.csrf(http.HandlerFunc(server.apiPreflight))))
	mux.Handle("GET /api/v1/uploads/recovery/{requestKey}", server.auth(http.HandlerFunc(server.apiRecovery)))
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
		writeAPIError(response, http.StatusServiceUnavailable, "NOT_READY", "문서 저장소가 준비되지 않았습니다.", true)
		return
	}
	if s.search != nil {
		if err := s.search.Ready(ctx); err != nil {
			writeAPIError(response, http.StatusServiceUnavailable, "NOT_READY", "검색 서비스가 준비되지 않았습니다.", true)
			return
		}
	}
	if s.worker != nil {
		if err := s.worker.Ready(ctx); err != nil {
			writeAPIError(response, http.StatusServiceUnavailable, "NOT_READY", "문서 처리 서비스가 준비되지 않았습니다.", true)
			return
		}
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "ready"})
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
		if _, ok, err := s.currentSession(request.Context(), request); err != nil {
			writeAPIError(response, http.StatusServiceUnavailable, "TEMPORARILY_UNAVAILABLE", "세션 저장소를 확인하세요.", true)
			return
		} else if !ok {
			writeAPIError(response, http.StatusUnauthorized, "SESSION_EXPIRED", "로그인 세션이 만료되었습니다.", false)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		current, ok, err := s.currentSession(request.Context(), request)
		if err != nil {
			writeAPIError(response, http.StatusServiceUnavailable, "TEMPORARILY_UNAVAILABLE", "세션 저장소를 확인하세요.", true)
			return
		}
		if !ok {
			writeAPIError(response, http.StatusUnauthorized, "SESSION_EXPIRED", "로그인 세션이 만료되었습니다.", false)
			return
		}
		provided := request.Header.Get("X-CSRF-Token")
		if provided == "" && strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data") {
			request.Body = http.MaxBytesReader(response, request.Body, knowledge.MaxUploadBytes+1024*1024)
			provided = request.FormValue("csrf")
		}
		if subtleString(provided, current.CSRFToken) == 0 {
			writeAPIError(response, http.StatusForbidden, "CSRF_REJECTED", "요청 검증에 실패했습니다.", false)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) currentSession(ctx context.Context, request *http.Request) (sessions.Record, bool, error) {
	cookie, err := request.Cookie(sessionCookie)
	if err != nil {
		return sessions.Record{}, false, nil
	}
	return s.sessions.Load(ctx, cookie.Value, time.Now())
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
			oldestKey, oldest = candidate, entry.lastSeen
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
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set("Referrer-Policy", "same-origin")
		response.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(response, request)
	})
}
