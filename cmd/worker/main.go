package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/embedding"
	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/objectstore"
	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/pdf"
	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/postgres"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/processing"
	"github.com/Merge42-SyncBase/syncbase-was/internal/platform/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("worker stopped", "error", err)
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
	modelPath, err := config.Required("SYNCBASE_MODEL_PATH")
	if err != nil {
		return err
	}
	tokenizerPath, err := config.Required("SYNCBASE_TOKENIZER_PATH")
	if err != nil {
		return err
	}
	runtimeLibrary, err := config.Required("SYNCBASE_ORT_LIBRARY_PATH")
	if err != nil {
		return err
	}
	pollInterval, err := config.Duration("SYNCBASE_WORKER_POLL_INTERVAL", time.Second)
	if err != nil {
		return err
	}
	workerID := config.Value("SYNCBASE_WORKER_ID", "worker-1")
	healthAddress := config.Value("SYNCBASE_WORKER_HEALTH_ADDR", ":8082")
	profile, _, err := config.Profile()
	if err != nil {
		return err
	}
	runtimeSHA256, err := config.RuntimeLibrarySHA256()
	if err != nil {
		return err
	}
	pool, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := postgres.NewStore(pool)
	if err := store.VerifyProfile(ctx, profile); err != nil {
		return err
	}
	originals, err := objectstore.New(originalRoot)
	if err != nil {
		return err
	}
	parser, err := pdf.New(ctx)
	if err != nil {
		return err
	}
	defer parser.Close()
	embedder, err := embedding.New(embedding.Config{
		ModelPath: modelPath, ModelSHA256: config.ModelSHA256,
		TokenizerPath: tokenizerPath, TokenizerSHA256: config.TokenizerSHA256,
		RuntimeLibraryPath: runtimeLibrary, RuntimeSHA256: runtimeSHA256,
	})
	if err != nil {
		return err
	}
	defer embedder.Close()
	processor := processing.New(store, originals, parser, embedder, profile)
	healthServer, healthListener, err := newReadinessServer(
		healthAddress,
		store,
		originals,
		parser,
		embedder,
	)
	if err != nil {
		return err
	}
	workerContext, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	healthErrors := make(chan error, 1)
	go func() {
		if err := healthServer.Serve(healthListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			healthErrors <- fmt.Errorf("worker readiness server: %w", err)
			stopWorker()
		}
	}()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = healthServer.Shutdown(shutdownContext)
	}()
	slog.Info("worker ready", "worker_id", workerID, "profile", profile.Fingerprint)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-healthErrors:
			return err
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := poll(workerContext, store, processor, workerID); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("worker poll deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-healthErrors:
			return err
		case <-ticker.C:
		}
	}
}

func poll(
	ctx context.Context,
	store *postgres.Store,
	processor *processing.Processor,
	workerID string,
) error {
	claimed, err := store.ClaimNext(ctx, workerID)
	if err != nil || claimed == nil {
		return err
	}
	slog.Info("processing run claimed",
		"run_id", claimed.RunID, "document_id", claimed.DocumentID,
		"version", claimed.Version, "fence", claimed.Fence,
	)
	processContext, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-processContext.Done():
				return
			case <-ticker.C:
				if err := store.Heartbeat(processContext, claimed.RunID, claimed.Fence, workerID); err != nil {
					slog.Warn("processing lease heartbeat failed", "run_id", claimed.RunID, "error", err)
					cancel()
					return
				}
			}
		}
	}()
	err = processor.Process(processContext, *claimed)
	cancel()
	<-heartbeatDone
	if err != nil {
		if errors.Is(err, knowledge.ErrStaleFence) {
			return err
		}
		slog.Warn("processing run failed", "run_id", claimed.RunID, "error", err)
		return nil
	}
	slog.Info("processing run activated", "run_id", claimed.RunID, "version", claimed.Version)
	return nil
}
