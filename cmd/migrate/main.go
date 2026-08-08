package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/postgres"
	"github.com/Merge42-SyncBase/syncbase-was/internal/platform/config"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
	slog.Info("schema and processing profile are ready")
}

func run(ctx context.Context) error {
	databaseURL, err := config.Required("SYNCBASE_DATABASE_URL")
	if err != nil {
		return err
	}
	profile, canonical, err := config.Profile()
	if err != nil {
		return err
	}
	pool, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return postgres.Migrate(ctx, pool, profile, canonical)
}
