// Package searchruntime exposes the deep WAS search module used by MCP.
//
// It deliberately hides PostgreSQL, profile, and vector-engine construction so
// the MCP repository depends on one stable interface instead of WAS internals.
package searchruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/embedding"
	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/postgres"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/search"
	"github.com/Merge42-SyncBase/syncbase-was/internal/platform/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrInvalidArgument reports an invalid search request or runtime configuration.
	ErrInvalidArgument = knowledge.ErrInvalidArgument
	// ErrProfileMismatch reports an incompatible processing or vector profile.
	ErrProfileMismatch = knowledge.ErrProfileMismatch
	// ErrTemporarilyUnavailable reports a retryable search dependency failure.
	ErrTemporarilyUnavailable = knowledge.ErrTemporarilyUnavailable
)

// Config contains the process-owned dependencies needed by the MCP search runtime.
// MinimumScore defaults to 0.62 when zero.
type Config struct {
	DatabaseURL        string
	ModelPath          string
	TokenizerPath      string
	RuntimeLibraryPath string
	PublicBaseURL      string
	MinimumScore       float64
}

// Hit is one ranked, page-grounded result from the active document version.
type Hit struct {
	Rank            int
	Score           float64
	DocumentID      string
	DocumentName    string
	VersionID       string
	DocumentVersion int
	PageNumber      int
	Snippet         string
	SourceURL       string
}

// Runtime owns the database pool, pinned vector engine, and search module.
type Runtime struct {
	pool      *pgxpool.Pool
	store     *postgres.Store
	profile   knowledge.Profile
	embedder  *embedding.E5
	search    *search.Service
	closeOnce sync.Once
	closeErr  error
}

// Open validates the immutable processing profile and opens the MCP search runtime.
func Open(ctx context.Context, runtimeConfig Config) (*Runtime, error) {
	if strings.TrimSpace(runtimeConfig.DatabaseURL) == "" ||
		strings.TrimSpace(runtimeConfig.ModelPath) == "" ||
		strings.TrimSpace(runtimeConfig.TokenizerPath) == "" ||
		strings.TrimSpace(runtimeConfig.RuntimeLibraryPath) == "" ||
		strings.TrimSpace(runtimeConfig.PublicBaseURL) == "" {
		return nil, fmt.Errorf("configure MCP search runtime: %w", ErrInvalidArgument)
	}
	minimumScore := runtimeConfig.MinimumScore
	if minimumScore == 0 {
		minimumScore = 0.62
	}
	profile, _, err := knowledge.NewProfile(
		config.ModelSHA256,
		config.TokenizerSHA256,
		config.ONNXRuntimeID,
		minimumScore,
	)
	if err != nil {
		return nil, fmt.Errorf("build processing profile: %w", err)
	}
	runtimeSHA256, err := config.RuntimeLibrarySHA256()
	if err != nil {
		return nil, err
	}
	pool, err := postgres.Open(ctx, runtimeConfig.DatabaseURL)
	if err != nil {
		return nil, err
	}
	store := postgres.NewStore(pool)
	if err := store.VerifyProfile(ctx, profile); err != nil {
		pool.Close()
		return nil, err
	}
	embedder, err := embedding.New(embedding.Config{
		ModelPath:          runtimeConfig.ModelPath,
		ModelSHA256:        config.ModelSHA256,
		TokenizerPath:      runtimeConfig.TokenizerPath,
		TokenizerSHA256:    config.TokenizerSHA256,
		RuntimeLibraryPath: runtimeConfig.RuntimeLibraryPath,
		RuntimeSHA256:      runtimeSHA256,
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	searchService, err := search.New(store, embedder, profile, runtimeConfig.PublicBaseURL)
	if err != nil {
		_ = embedder.Close()
		pool.Close()
		return nil, err
	}
	return &Runtime{
		pool:     pool,
		store:    store,
		profile:  profile,
		embedder: embedder,
		search:   searchService,
	}, nil
}

// Documents validates, embeds, and searches only active document versions.
func (r *Runtime) Documents(ctx context.Context, query string, limit int) ([]Hit, error) {
	hits, err := r.search.Documents(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	result := make([]Hit, len(hits))
	for index, hit := range hits {
		result[index] = Hit{
			Rank:            hit.Rank,
			Score:           hit.Score,
			DocumentID:      hit.DocumentID.String(),
			DocumentName:    hit.DocumentName,
			VersionID:       hit.VersionID.String(),
			DocumentVersion: hit.DocumentVersion,
			PageNumber:      hit.PageNumber,
			Snippet:         hit.Snippet,
			SourceURL:       hit.SourceURL,
		}
	}
	return result, nil
}

// Ready verifies the database profile and local query embedder without mutation.
func (r *Runtime) Ready(ctx context.Context) error {
	if err := r.store.Ready(ctx); err != nil {
		return err
	}
	if err := r.store.VerifyProfile(ctx, r.profile); err != nil {
		return err
	}
	return r.search.Ready(ctx)
}

// Close releases the vector runtime and database pool. It is safe to call repeatedly.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.embedder.Close()
		r.pool.Close()
	})
	return r.closeErr
}

// IsRetryable reports whether err represents a dependency outage safe to retry.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrTemporarilyUnavailable) || errors.Is(err, context.DeadlineExceeded)
}
