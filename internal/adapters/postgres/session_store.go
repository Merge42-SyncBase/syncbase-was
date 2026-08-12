package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/sessions"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionStore persists only a SHA-256 digest of each browser bearer token.
type SessionStore struct {
	pool *pgxpool.Pool
}

// NewSessionStore returns a PostgreSQL-backed browser session store.
func NewSessionStore(pool *pgxpool.Pool) *SessionStore {
	return &SessionStore{pool: pool}
}

// Create writes a new browser session without storing the raw bearer token.
func (s *SessionStore) Create(ctx context.Context, token string, record sessions.Record) error {
	if token == "" || record.CSRFToken == "" || record.ExpiresAt.IsZero() {
		return fmt.Errorf("create session: invalid session")
	}
	digest := sessionDigest(token)
	if _, err := s.pool.Exec(ctx, "DELETE FROM syncbase.browser_session WHERE expires_at <= clock_timestamp()"); err != nil {
		return fmt.Errorf("prune expired sessions: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO syncbase.browser_session(token_hash, csrf_token, expires_at)
		VALUES ($1, $2, $3)`, digest[:], record.CSRFToken, record.ExpiresAt); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Load returns an unexpired session for token. The query atomically rejects
// expired records so multiple web instances share the same expiry decision.
func (s *SessionStore) Load(ctx context.Context, token string, now time.Time) (sessions.Record, bool, error) {
	if token == "" {
		return sessions.Record{}, false, nil
	}
	digest := sessionDigest(token)
	var record sessions.Record
	err := s.pool.QueryRow(ctx, `
		SELECT csrf_token, expires_at
		FROM syncbase.browser_session
		WHERE token_hash=$1 AND expires_at > $2`, digest[:], now).Scan(&record.CSRFToken, &record.ExpiresAt)
	if err == nil {
		return record, true, nil
	}
	if err == pgx.ErrNoRows {
		return sessions.Record{}, false, nil
	}
	return sessions.Record{}, false, fmt.Errorf("load session: %w", err)
}

// Delete revokes token immediately. A missing token is already revoked.
func (s *SessionStore) Delete(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	digest := sessionDigest(token)
	if _, err := s.pool.Exec(ctx, "DELETE FROM syncbase.browser_session WHERE token_hash=$1", digest[:]); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func sessionDigest(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}
