package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/objectstore"
	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/pdf"
	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/postgres"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/documents"
	"github.com/Merge42-SyncBase/syncbase-was/internal/platform/config"
	"github.com/Merge42-SyncBase/syncbase-was/internal/transport/webapp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("web server stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	databaseURL, err := config.Required("SYNCBASE_DATABASE_URL")
	if err != nil {
		return err
	}
	originalRoot, err := config.Required("SYNCBASE_ORIGINAL_ROOT")
	if err != nil {
		return err
	}
	adminHash, err := config.Required("SYNCBASE_ADMIN_PASSWORD_BCRYPT")
	if err != nil {
		return err
	}
	mcpURL, err := config.Required("SYNCBASE_MCP_URL")
	if err != nil {
		return err
	}
	mcpTokenFile, err := config.Required("SYNCBASE_MCP_TOKEN_FILE")
	if err != nil {
		return err
	}
	mcpTokenBytes, err := os.ReadFile(mcpTokenFile)
	if err != nil {
		return fmt.Errorf("read MCP client token: %w", err)
	}
	mcpToken := strings.TrimSpace(string(mcpTokenBytes))
	if mcpToken == "" {
		return errors.New("MCP client token is empty")
	}
	cookieSecure, err := config.Bool("SYNCBASE_COOKIE_SECURE", true)
	if err != nil {
		return err
	}
	workerReadyURL, err := config.Required("SYNCBASE_WORKER_READY_URL")
	if err != nil {
		return err
	}
	pool, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	originals, err := objectstore.New(originalRoot)
	if err != nil {
		return err
	}
	parser, err := pdf.New(ctx)
	if err != nil {
		return err
	}
	defer parser.Close()
	documentService, err := documents.New(postgres.NewStore(pool), originals, parser)
	if err != nil {
		return err
	}
	handler, err := webapp.New(webapp.Config{
		AdminUsername:       config.Value("SYNCBASE_ADMIN_USERNAME", "admin"),
		AdminPasswordBcrypt: adminHash, CookieSecure: cookieSecure,
		MCPURL: mcpURL, MCPToken: mcpToken, WorkerReadyURL: workerReadyURL,
		Sessions: postgres.NewSessionStore(pool),
	}, documentService)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: config.Value("SYNCBASE_WEB_ADDR", ":8080"), Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 120 * time.Second,
		WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()
	slog.Info("web server ready", "address", server.Addr)
	err = server.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return ctx.Err()
}
