package postgres

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/001_syncbase.sql
var initialSchema string

//go:embed migrations/002_processing_step_attempt.sql
var processingAttemptSchema string

//go:embed migrations/003_fenced_processing_attempt.sql
var fencedProcessingAttemptSchema string

type migration struct {
	version int
	name    string
	sql     string
}

var migrations = []migration{
	{version: 1, name: "syncbase", sql: initialSchema},
	{version: 2, name: "processing_step_attempt", sql: processingAttemptSchema},
	{version: 3, name: "fenced_processing_attempt", sql: fencedProcessingAttemptSchema},
}

// Open returns a bounded PostgreSQL connection pool after a successful ping.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	config.MaxConns = 8
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate installs the stable schema and verifies the immutable processing profile.
func Migrate(ctx context.Context, pool *pgxpool.Pool, profile knowledge.Profile, canonical string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7341261038839324265)"); err != nil {
		return fmt.Errorf("lock migration: %w", err)
	}
	if _, err := tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("install vector extension: %w", err)
	}
	if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS syncbase"); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	var managedSchemaExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1
		FROM pg_catalog.pg_class relation
		JOIN pg_catalog.pg_namespace namespace ON namespace.oid=relation.relnamespace
		WHERE namespace.nspname='syncbase'
		  AND relation.relkind IN ('r','p')
		  AND relation.relname <> 'schema_migration'
	)`).Scan(&managedSchemaExists); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	var migrationLedgerExists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('syncbase.schema_migration') IS NOT NULL`).Scan(&migrationLedgerExists); err != nil {
		return fmt.Errorf("inspect migration ledger: %w", err)
	}
	if managedSchemaExists && !migrationLedgerExists {
		return errors.New("syncbase schema exists without a migration ledger; refusing an unverified baseline")
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS syncbase.schema_migration (
			version integer PRIMARY KEY CHECK (version > 0),
			name text NOT NULL UNIQUE CHECK (name <> ''),
			checksum text NOT NULL CHECK (checksum ~ '^[0-9a-f]{64}$'),
			applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
		)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO syncbase, public"); err != nil {
		return fmt.Errorf("set migration schema: %w", err)
	}
	for _, item := range migrations {
		if err := applyMigration(ctx, tx, item); err != nil {
			return err
		}
	}

	var active string
	err = tx.QueryRow(ctx, "SELECT fingerprint FROM syncbase.processing_profile WHERE active = true FOR UPDATE").Scan(&active)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO syncbase.processing_profile(
				fingerprint, canonical_json, parser_id, chunker_id, embedding_model_id,
				vector_dimension, distance, minimum_score, active
			) VALUES ($1, $2::jsonb, $3, $4, $5, $6, $7, $8, true)`,
			profile.Fingerprint, canonical, profile.ParserID, profile.ChunkerID,
			profile.EmbeddingModelID, profile.VectorDimension, profile.Distance, profile.MinimumScore,
		)
		if err != nil {
			return fmt.Errorf("insert processing profile: %w", err)
		}
	case err != nil:
		return fmt.Errorf("load processing profile: %w", err)
	case active != profile.Fingerprint:
		return fmt.Errorf("%w: active=%s configured=%s", knowledge.ErrProfileMismatch, active, profile.Fingerprint)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

func applyMigration(ctx context.Context, tx pgx.Tx, item migration) error {
	digest := sha256.Sum256([]byte(item.sql))
	checksum := hex.EncodeToString(digest[:])
	var storedChecksum string
	err := tx.QueryRow(ctx, `
		SELECT checksum FROM syncbase.schema_migration WHERE version=$1`, item.version).Scan(&storedChecksum)
	switch {
	case err == nil && storedChecksum != checksum:
		return fmt.Errorf("migration %03d_%s checksum mismatch: stored=%s configured=%s",
			item.version, item.name, storedChecksum, checksum)
	case err == nil:
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("load migration %03d_%s: %w", item.version, item.name, err)
	}
	if _, err := tx.Exec(ctx, item.sql); err != nil {
		return fmt.Errorf("apply migration %03d_%s: %w", item.version, item.name, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO syncbase.schema_migration(version,name,checksum)
		VALUES ($1,$2,$3)`, item.version, item.name, checksum); err != nil {
		return fmt.Errorf("record migration %03d_%s: %w", item.version, item.name, err)
	}
	return nil
}
