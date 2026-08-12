package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/postgres"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/sessions"
)

func TestSessionStorePersistsDigestAndRevocation(t *testing.T) {
	databaseURL := os.Getenv("SYNCBASE_TEST_DB_URL")
	if databaseURL == "" {
		t.Skip("SYNCBASE_TEST_DB_URL is not set")
	}
	ctx := context.Background()
	pool, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	profile, canonical := testProfile(t)
	if err := postgres.Migrate(ctx, pool, profile, canonical); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	store := postgres.NewSessionStore(pool)
	token := "bearer-token-must-not-be-persisted"
	record := sessions.Record{CSRFToken: "0123456789abcdef0123456789abcdef", ExpiresAt: time.Now().Add(time.Minute)}
	if err := store.Create(ctx, token, record); err != nil {
		t.Fatalf("Create: %v", err)
	}
	loaded, found, err := store.Load(ctx, token, time.Now())
	if err != nil || !found || loaded.CSRFToken != record.CSRFToken ||
		loaded.ExpiresAt.Sub(record.ExpiresAt).Abs() > time.Microsecond {
		t.Fatalf("Load = (%+v, %v, %v), want (%+v, true, nil)", loaded, found, err, record)
	}
	digest := sha256.Sum256([]byte(token))
	var storedDigest string
	if err := pool.QueryRow(ctx, `
		SELECT encode(token_hash, 'hex')
		FROM syncbase.browser_session
		WHERE token_hash=$1`, digest[:]).Scan(&storedDigest); err != nil {
		t.Fatalf("query stored digest: %v", err)
	}
	if storedDigest != hex.EncodeToString(digest[:]) || storedDigest == token {
		t.Fatalf("stored token digest=%q", storedDigest)
	}
	if err := store.Delete(ctx, token); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, err := store.Load(ctx, token, time.Now()); err != nil || found {
		t.Fatalf("Load after Delete = (found=%v, err=%v), want false, nil", found, err)
	}
}

func testProfile(t *testing.T) (knowledge.Profile, string) {
	t.Helper()
	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0", 0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	return profile, canonical
}
