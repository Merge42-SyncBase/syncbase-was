package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/postgres"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInternalFailureRetriesSameRunThenExhausts(t *testing.T) {
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

	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	resetStoreFixtures(t, ctx, pool)
	store := postgres.NewStore(pool)
	name, err := knowledge.NewDocumentName("잠재 결함: INTERNAL 리트라이")
	if err != nil {
		t.Fatalf("NewDocumentName: %v", err)
	}
	registered, err := store.Register(ctx, knowledge.RegisterCommand{
		RequestKey:       "internal-retry-v1",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     name,
		ContentSHA256:    "9fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ByteSize:         1234,
		OriginalFileName: "internal-retry.pdf",
		StorageKey:       "9f/e7/9fe7058f.pdf",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	first, err := store.ClaimNext(ctx, "worker-internal-test")
	if err != nil || first == nil {
		t.Fatalf("ClaimNext first: run=%+v err=%v", first, err)
	}
	if first.RunID != registered.RunID || first.AutomaticAttempt != 1 {
		t.Fatalf("first claim = %+v, want run=%s attempt=1", first, registered.RunID)
	}
	if err := store.Fail(ctx, *first, knowledge.StageParse, "INTERNAL"); err != nil {
		t.Fatalf("Fail first INTERNAL attempt: %v", err)
	}

	second := waitForClaim(t, ctx, store, "worker-internal-test", 2*time.Second)
	if second.RunID != first.RunID || second.AutomaticAttempt != 2 {
		t.Fatalf("second claim = %+v, want same run attempt=2 (INTERNAL should retry)", second)
	}
	if err := store.Fail(ctx, *second, knowledge.StageParse, "INTERNAL"); err != nil {
		t.Fatalf("Fail second INTERNAL attempt: %v", err)
	}

	third := waitForClaim(t, ctx, store, "worker-internal-test", 6*time.Second)
	if third.RunID != first.RunID || third.AutomaticAttempt != 3 {
		t.Fatalf("third claim = %+v, want same run attempt=3", third)
	}
	if err := store.Fail(ctx, *third, knowledge.StageParse, "INTERNAL"); err != nil {
		t.Fatalf("Fail exhausted INTERNAL attempt: %v", err)
	}
	details, err := store.GetDocument(ctx, registered.DocumentID)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if len(details.Versions) != 1 {
		t.Fatalf("versions=%d, want 1", len(details.Versions))
	}
	version := details.Versions[0]
	if version.Status != knowledge.VersionFailed {
		t.Fatalf("exhausted INTERNAL version status = %s, want FAILED", version.Status)
	}
	if version.ErrorCode != "TRANSIENT_EXHAUSTED" {
		t.Fatalf("exhausted INTERNAL version error_code = %s, want TRANSIENT_EXHAUSTED", version.ErrorCode)
	}
	if !version.ManualRetryAllowed {
		t.Fatalf("exhausted INTERNAL version should allow manual retry, got %+v", version)
	}
	if version.AutomaticAttempts != 3 {
		t.Fatalf("exhausted INTERNAL version attempts = %d, want 3", version.AutomaticAttempts)
	}
}

func TestTransientFailureRetriesSameRunThreeTimesBeforeManualRecovery(t *testing.T) {
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

	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	resetStoreFixtures(t, ctx, pool)
	store := postgres.NewStore(pool)
	name, err := knowledge.NewDocumentName("자동 재시도 계약")
	if err != nil {
		t.Fatalf("NewDocumentName: %v", err)
	}
	registered, err := store.Register(ctx, knowledge.RegisterCommand{
		RequestKey:       "automatic-retry-v1",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     name,
		ContentSHA256:    "9fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ByteSize:         1234,
		OriginalFileName: "automatic-retry.pdf",
		StorageKey:       "9f/e7/9fe7058f.pdf",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	first, err := store.ClaimNext(ctx, "worker-retry-test")
	if err != nil || first == nil {
		t.Fatalf("ClaimNext first: run=%+v err=%v", first, err)
	}
	if first.RunID != registered.RunID || first.AutomaticAttempt != 1 {
		t.Fatalf("first claim = %+v, want run=%s attempt=1", first, registered.RunID)
	}
	if err := store.Fail(ctx, *first, knowledge.StageEmbed, "TEMPORARILY_UNAVAILABLE"); err != nil {
		t.Fatalf("Fail first transient attempt: %v", err)
	}
	if immediate, err := store.ClaimNext(ctx, "worker-retry-test"); err != nil || immediate != nil {
		t.Fatalf("claim before first backoff = %+v, err=%v; want nil", immediate, err)
	}

	second := waitForClaim(t, ctx, store, "worker-retry-test", 2*time.Second)
	if second.RunID != first.RunID || second.AutomaticAttempt != 2 {
		t.Fatalf("second claim = %+v, want same run attempt=2", second)
	}
	if err := store.Fail(ctx, *second, knowledge.StageEmbed, "TEMPORARILY_UNAVAILABLE"); err != nil {
		t.Fatalf("Fail second transient attempt: %v", err)
	}

	third := waitForClaim(t, ctx, store, "worker-retry-test", 6*time.Second)
	if third.RunID != first.RunID || third.AutomaticAttempt != 3 {
		t.Fatalf("third claim = %+v, want same run attempt=3", third)
	}
	if err := store.Fail(ctx, *third, knowledge.StageEmbed, "TEMPORARILY_UNAVAILABLE"); err != nil {
		t.Fatalf("Fail exhausted transient attempt: %v", err)
	}
	details, err := store.GetDocument(ctx, registered.DocumentID)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if len(details.Versions) != 1 {
		t.Fatalf("versions=%d, want 1", len(details.Versions))
	}
	version := details.Versions[0]
	if version.Status != knowledge.VersionFailed || version.ErrorCode != "TRANSIENT_EXHAUSTED" ||
		version.AutomaticAttempts != 3 || !version.ManualRetryAllowed {
		t.Fatalf("exhausted version = %+v", version)
	}
	var recordedAttempts int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM syncbase.processing_step_attempt
		WHERE run_id=$1 AND stage='EMBED'
		  AND outcome IN ('RETRY_SCHEDULED','FAILED')`, registered.RunID).Scan(&recordedAttempts); err != nil {
		t.Fatalf("query processing attempts: %v", err)
	}
	if recordedAttempts != 3 {
		t.Fatalf("recorded EMBED attempts = %d, want 3", recordedAttempts)
	}
}

func TestSetStageCompletesThePreviousAttemptBeforeStartingTheNext(t *testing.T) {
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

	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	resetStoreFixtures(t, ctx, pool)
	store := postgres.NewStore(pool)
	name, err := knowledge.NewDocumentName("단계 감사 이력 계약")
	if err != nil {
		t.Fatalf("NewDocumentName: %v", err)
	}
	registered, err := store.Register(ctx, knowledge.RegisterCommand{
		RequestKey:       "stage-audit-v1",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     name,
		ContentSHA256:    "8fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ByteSize:         1234,
		OriginalFileName: "stage-audit.pdf",
		StorageKey:       "8f/e7/8fe7058f.pdf",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	claimed, err := store.ClaimNext(ctx, "worker-stage-audit-test")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: run=%+v err=%v", claimed, err)
	}
	if err := store.SetStage(ctx, claimed.RunID, claimed.Fence, knowledge.StageParse); err != nil {
		t.Fatalf("SetStage PARSE: %v", err)
	}
	if err := store.SetStage(ctx, claimed.RunID, claimed.Fence, knowledge.StageChunk); err != nil {
		t.Fatalf("SetStage CHUNK: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT stage,outcome,finished_at IS NOT NULL
		FROM syncbase.processing_step_attempt
		WHERE run_id=$1 AND automatic_attempt=1
		ORDER BY CASE stage WHEN 'METADATA' THEN 1 WHEN 'PARSE' THEN 2 WHEN 'CHUNK' THEN 3 END`,
		registered.RunID)
	if err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	defer rows.Close()
	want := []struct {
		stage    knowledge.Stage
		outcome  string
		finished bool
	}{
		{knowledge.StageMetadata, "SUCCEEDED", true},
		{knowledge.StageParse, "SUCCEEDED", true},
		{knowledge.StageChunk, "RUNNING", false},
	}
	for index := range want {
		if !rows.Next() {
			t.Fatalf("attempt row %d missing", index)
		}
		var stage knowledge.Stage
		var outcome string
		var finished bool
		if err := rows.Scan(&stage, &outcome, &finished); err != nil {
			t.Fatalf("scan attempt %d: %v", index, err)
		}
		if stage != want[index].stage || outcome != want[index].outcome || finished != want[index].finished {
			t.Fatalf("attempt %d = (%s,%s,%v), want (%s,%s,%v)", index,
				stage, outcome, finished, want[index].stage, want[index].outcome, want[index].finished)
		}
	}
	if rows.Next() {
		t.Fatal("unexpected extra attempt row")
	}
}

func TestMigrateRecordsAndVerifiesChecksums(t *testing.T) {
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
	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE syncbase.processing_profile
			SET canonical_json=$1::jsonb, provider=$2
			WHERE active=true`, canonical, profile.Provider); err != nil {
			t.Errorf("restore processing profile fixture: %v", err)
		}
	})
	var migrations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM syncbase.schema_migration
		WHERE checksum ~ '^[0-9a-f]{64}$'`).Scan(&migrations); err != nil {
		t.Fatalf("query migration ledger: %v", err)
	}
	if migrations != 5 {
		t.Fatalf("migration ledger rows = %d, want 5", migrations)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE syncbase.processing_profile
		SET canonical_json=jsonb_set(canonical_json, '{provider}', '"tampered"')
		WHERE active=true`); err != nil {
		t.Fatalf("tamper profile canonical fixture: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); !errors.Is(err, knowledge.ErrProfileMismatch) {
		t.Fatalf("Migrate after canonical profile tamper error = %v, want ErrProfileMismatch", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE syncbase.processing_profile SET canonical_json=$1::jsonb WHERE active=true`, canonical); err != nil {
		t.Fatalf("restore profile canonical fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE syncbase.processing_profile SET provider='tampered' WHERE active=true`); err != nil {
		t.Fatalf("tamper profile metadata fixture: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); !errors.Is(err, knowledge.ErrProfileMismatch) {
		t.Fatalf("Migrate after metadata profile tamper error = %v, want ErrProfileMismatch", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE syncbase.processing_profile SET provider=$1 WHERE active=true`, profile.Provider); err != nil {
		t.Fatalf("restore profile metadata fixture: %v", err)
	}
	var originalChecksum string
	if err := pool.QueryRow(ctx, `
		SELECT checksum FROM syncbase.schema_migration WHERE version=1`).Scan(&originalChecksum); err != nil {
		t.Fatalf("load migration checksum fixture: %v", err)
	}
	defer func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE syncbase.schema_migration SET checksum=$1 WHERE version=1`, originalChecksum); err != nil {
			t.Errorf("restore migration checksum fixture: %v", err)
		}
	}()
	if _, err := pool.Exec(ctx, `
		UPDATE syncbase.schema_migration SET checksum=repeat('0',64) WHERE version=1`); err != nil {
		t.Fatalf("tamper migration checksum fixture: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); err == nil {
		t.Fatal("Migrate after checksum tamper = nil, want error")
	}
}

func TestMigrateRejectsAnUnverifiedExistingSchema(t *testing.T) {
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
	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext := context.Background()
		if _, cleanupErr := pool.Exec(cleanupContext, "DROP SCHEMA IF EXISTS syncbase CASCADE"); cleanupErr != nil {
			t.Errorf("drop unmanaged schema: %v", cleanupErr)
			return
		}
		if cleanupErr := postgres.Migrate(cleanupContext, pool, profile, canonical); cleanupErr != nil {
			t.Errorf("restore managed schema: %v", cleanupErr)
		}
	})
	if _, err := pool.Exec(ctx, `
		DROP SCHEMA IF EXISTS syncbase CASCADE;
		CREATE SCHEMA syncbase;
		CREATE TABLE syncbase.document(id uuid PRIMARY KEY)`); err != nil {
		t.Fatalf("create unmanaged schema: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); err == nil ||
		!strings.Contains(err.Error(), "without a migration ledger") {
		t.Fatalf("Migrate unmanaged schema error = %v, want baseline rejection", err)
	}
}

func TestLeaseReclaimPreservesAttemptHistoryAndRejectsStaleWrites(t *testing.T) {
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
	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	resetStoreFixtures(t, ctx, pool)
	store := postgres.NewStore(pool)
	name, err := knowledge.NewDocumentName("lease fencing contract")
	if err != nil {
		t.Fatalf("NewDocumentName: %v", err)
	}
	registered, err := store.Register(ctx, knowledge.RegisterCommand{
		RequestKey:       "lease-fencing-v1",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     name,
		ContentSHA256:    "1fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ByteSize:         1234,
		OriginalFileName: "lease-fencing.pdf",
		StorageKey:       "1f/e7/1fe7058f.pdf",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	first, err := store.ClaimNext(ctx, "worker-old")
	if err != nil || first == nil {
		t.Fatalf("ClaimNext first: run=%+v err=%v", first, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE syncbase.processing_run
		SET lease_until=clock_timestamp()-interval '1 second'
		WHERE id=$1`, registered.RunID); err != nil {
		t.Fatalf("expire first lease: %v", err)
	}
	checkpoint := knowledge.Checkpoint{
		RunID: first.RunID, Stage: knowledge.StageParse,
		InputSHA256:   first.ContentSHA256,
		OutputSHA256:  "2fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		FormatVersion: "syncbase-json-v1", ArtifactKey: "2f/e7/checkpoint", ArtifactSize: 1,
		FencingToken: first.Fence,
	}
	if err := store.SaveCheckpoint(ctx, *first, checkpoint); !errors.Is(err, knowledge.ErrStaleFence) {
		t.Fatalf("SaveCheckpoint with expired lease error = %v, want ErrStaleFence", err)
	}
	if err := store.SetStage(ctx, first.RunID, first.Fence, knowledge.StageParse); !errors.Is(err, knowledge.ErrStaleFence) {
		t.Fatalf("SetStage with expired lease error = %v, want ErrStaleFence", err)
	}
	if err := store.Heartbeat(ctx, first.RunID, first.Fence, "worker-old"); !errors.Is(err, knowledge.ErrStaleFence) {
		t.Fatalf("Heartbeat with expired lease error = %v, want ErrStaleFence", err)
	}
	second, err := store.ClaimNext(ctx, "worker-new")
	if err != nil || second == nil {
		t.Fatalf("ClaimNext reclaimed: run=%+v err=%v", second, err)
	}
	if second.RunID != first.RunID || second.Fence == first.Fence || second.AutomaticAttempt != first.AutomaticAttempt {
		t.Fatalf("reclaimed run = %+v, first = %+v", second, first)
	}

	rows, err := pool.Query(ctx, `
		SELECT fencing_token,outcome,finished_at IS NOT NULL
		FROM syncbase.processing_step_attempt
		WHERE run_id=$1 AND stage='METADATA'
		ORDER BY fencing_token`, registered.RunID)
	if err != nil {
		t.Fatalf("query reclaimed attempts: %v", err)
	}
	defer rows.Close()
	want := []struct {
		fence    int64
		outcome  string
		finished bool
	}{
		{fence: first.Fence, outcome: "SUPERSEDED", finished: true},
		{fence: second.Fence, outcome: "RUNNING", finished: false},
	}
	for index, expected := range want {
		if !rows.Next() {
			t.Fatalf("attempt %d missing", index)
		}
		var fence int64
		var outcome string
		var finished bool
		if err := rows.Scan(&fence, &outcome, &finished); err != nil {
			t.Fatalf("scan attempt %d: %v", index, err)
		}
		if fence != expected.fence || outcome != expected.outcome || finished != expected.finished {
			t.Fatalf("attempt %d = (%d,%s,%v), want (%d,%s,%v)", index,
				fence, outcome, finished, expected.fence, expected.outcome, expected.finished)
		}
	}
	if rows.Next() {
		t.Fatal("unexpected additional reclaimed attempt")
	}

	if err := store.SaveCheckpoint(ctx, *first, checkpoint); !errors.Is(err, knowledge.ErrStaleFence) {
		t.Fatalf("SaveCheckpoint with stale fence error = %v, want ErrStaleFence", err)
	}
	vector := make([]float32, knowledge.VectorDimension)
	vector[0] = 1
	if err := store.StoreChunks(ctx, *first, []knowledge.IndexedChunk{{
		Chunk:   knowledge.Chunk{Index: 0, PageNumber: 1, Text: "stale content"},
		Snippet: "stale content", Embedding: vector,
	}}); !errors.Is(err, knowledge.ErrStaleFence) {
		t.Fatalf("StoreChunks with stale fence error = %v, want ErrStaleFence", err)
	}
}

func waitForClaim(
	t *testing.T,
	ctx context.Context,
	store *postgres.Store,
	workerID string,
	timeout time.Duration,
) *knowledge.ClaimedRun {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		claimed, err := store.ClaimNext(ctx, workerID)
		if err != nil {
			t.Fatalf("ClaimNext: %v", err)
		}
		if claimed != nil {
			return claimed
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run was not claimable within %s", timeout)
	return nil
}

func TestRegisterDocumentIsAtomicAndIdempotent(t *testing.T) {
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

	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err := postgres.Migrate(ctx, pool, profile, canonical); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	store := postgres.NewStore(pool)
	resetStoreFixtures(t, ctx, pool)
	name, err := knowledge.NewDocumentName("보안 정책")
	if err != nil {
		t.Fatalf("NewDocumentName: %v", err)
	}
	command := knowledge.RegisterCommand{
		RequestKey:       "request-register-v1",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     name,
		ContentSHA256:    "4fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ByteSize:         1234,
		OriginalFileName: "security-policy.pdf",
		StorageKey:       "4f/e7/4fe7058f.pdf",
	}
	first, err := store.Register(ctx, command)
	if err != nil {
		t.Fatalf("Register first: %v", err)
	}
	second, err := store.Register(ctx, command)
	if err != nil {
		t.Fatalf("Register repeated: %v", err)
	}
	if first.DocumentID != second.DocumentID || first.VersionID != second.VersionID || first.RunID != second.RunID {
		t.Fatalf("repeated registration diverged: first=%+v second=%+v", first, second)
	}
	if !second.Recovered {
		t.Fatal("repeated registration Recovered = false, want true")
	}

	documents, err := store.ListDocuments(ctx, 50, 0)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if got, want := len(documents), 1; got != want {
		t.Fatalf("document count = %d, want %d", got, want)
	}
	if got, want := documents[0].LatestStatus, knowledge.VersionQueued; got != want {
		t.Fatalf("latest status = %s, want %s", got, want)
	}

	vector := make([]float32, knowledge.VectorDimension)
	vector[0] = 1
	claimedV1, err := store.ClaimNext(ctx, "worker-test")
	if err != nil {
		t.Fatalf("ClaimNext v1: %v", err)
	}
	if claimedV1 == nil || claimedV1.RunID != first.RunID {
		t.Fatalf("claimed v1 = %+v, want run %s", claimedV1, first.RunID)
	}
	if err := store.SetStage(ctx, claimedV1.RunID, claimedV1.Fence, knowledge.StageStore); err != nil {
		t.Fatalf("SetStage v1: %v", err)
	}
	if err := store.StoreChunks(ctx, *claimedV1, []knowledge.IndexedChunk{{
		Chunk:     knowledge.Chunk{Index: 0, PageNumber: 1, Text: "이전 보안 정책"},
		Snippet:   "이전 보안 정책",
		Embedding: vector,
	}}); err != nil {
		t.Fatalf("StoreChunks v1: %v", err)
	}
	if err := store.Finalize(ctx, *claimedV1, 1, 1); err != nil {
		t.Fatalf("Finalize v1: %v", err)
	}
	hits, err := store.Search(ctx, profile, vector, 5, "https://docs.example.test")
	if err != nil {
		t.Fatalf("Search v1: %v", err)
	}
	if got, want := len(hits), 1; got != want {
		t.Fatalf("v1 search result count = %d, want %d", got, want)
	}
	if got, want := hits[0].SourceURL, "https://docs.example.test/sources/"+first.DocumentID.String()+"/versions/1?page=1"; got != want {
		t.Fatalf("v1 source URL = %q, want %q", got, want)
	}
	if hits[0].StorageKey != claimedV1.StorageKey || hits[0].ContentSHA256 != claimedV1.ContentSHA256 {
		t.Fatalf("v1 private source identity = key %q digest %q, want key %q digest %q",
			hits[0].StorageKey, hits[0].ContentSHA256, claimedV1.StorageKey, claimedV1.ContentSHA256)
	}

	v2Command := knowledge.RegisterCommand{
		RequestKey:       "request-register-v2",
		Operation:        knowledge.RegisterNewVersion,
		TargetDocumentID: &first.DocumentID,
		DocumentName:     name,
		ContentSHA256:    "7ff686cd2bf183215f3bcbfa3a7bf13ac1398ba4c11537d635962f1455e7f4b7",
		ByteSize:         2345,
		OriginalFileName: "security-policy-v2.pdf",
		StorageKey:       "7f/f6/7ff686cd.pdf",
	}
	v2, err := store.Register(ctx, v2Command)
	if err != nil {
		t.Fatalf("Register v2: %v", err)
	}
	hits, err = store.Search(ctx, profile, vector, 5, "https://docs.example.test")
	if err != nil {
		t.Fatalf("Search while v2 queued: %v", err)
	}
	if got, want := hits[0].DocumentVersion, 1; got != want {
		t.Fatalf("queued v2 changed active search version to %d, want %d", got, want)
	}

	claimedV2, err := store.ClaimNext(ctx, "worker-test")
	if err != nil {
		t.Fatalf("ClaimNext v2: %v", err)
	}
	if claimedV2 == nil || claimedV2.RunID != v2.RunID {
		t.Fatalf("claimed v2 = %+v, want run %s", claimedV2, v2.RunID)
	}
	if err := store.StoreChunks(ctx, *claimedV2, []knowledge.IndexedChunk{{
		Chunk:     knowledge.Chunk{Index: 0, PageNumber: 2, Text: "개정 보안 정책"},
		Snippet:   "개정 보안 정책",
		Embedding: vector,
	}}); err != nil {
		t.Fatalf("StoreChunks v2: %v", err)
	}
	if err := store.Finalize(ctx, *claimedV2, 2, 1); err != nil {
		t.Fatalf("Finalize v2: %v", err)
	}
	hits, err = store.Search(ctx, profile, vector, 5, "https://docs.example.test")
	if err != nil {
		t.Fatalf("Search v2: %v", err)
	}
	if got, want := len(hits), 1; got != want {
		t.Fatalf("v2 search result count = %d, want %d", got, want)
	}
	if got, want := hits[0].DocumentVersion, 2; got != want {
		t.Fatalf("active search version = %d, want %d", got, want)
	}
	if got, want := hits[0].PageNumber, 2; got != want {
		t.Fatalf("active search page = %d, want %d", got, want)
	}

	v3Command := knowledge.RegisterCommand{
		RequestKey:       "request-register-v3",
		Operation:        knowledge.RegisterNewVersion,
		TargetDocumentID: &first.DocumentID,
		DocumentName:     name,
		ContentSHA256:    "8ff686cd2bf183215f3bcbfa3a7bf13ac1398ba4c11537d635962f1455e7f4b7",
		ByteSize:         3456,
		OriginalFileName: "security-policy-v3.pdf",
		StorageKey:       "8f/f6/8ff686cd.pdf",
	}
	v3, err := store.Register(ctx, v3Command)
	if err != nil {
		t.Fatalf("Register v3: %v", err)
	}
	failedV3, err := store.ClaimNext(ctx, "worker-test")
	if err != nil || failedV3 == nil || failedV3.RunID != v3.RunID {
		t.Fatalf("ClaimNext failed v3: run=%+v err=%v", failedV3, err)
	}
	if err := store.Fail(ctx, *failedV3, knowledge.StageParse, "INVALID_INPUT"); err != nil {
		t.Fatalf("Fail v3: %v", err)
	}
	if _, err := store.Retry(ctx, failedV3.RunID, "retry-invalid-v3"); !errors.Is(err, knowledge.ErrInvalidArgument) {
		t.Fatalf("Retry permanent failure error = %v, want ErrInvalidArgument", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE syncbase.processing_run SET error_code='TRANSIENT_EXHAUSTED'
		WHERE id=$1`, failedV3.RunID); err != nil {
		t.Fatalf("prepare exhausted retry fixture: %v", err)
	}
	retryRunID, err := store.Retry(ctx, failedV3.RunID, "retry-v3-once")
	if err != nil {
		t.Fatalf("Retry v3: %v", err)
	}
	recoveredRetryRunID, err := store.Retry(ctx, failedV3.RunID, "retry-v3-once")
	if err != nil {
		t.Fatalf("Retry v3 repeated: %v", err)
	}
	if retryRunID != recoveredRetryRunID {
		t.Fatalf("retry idempotency diverged: first=%s repeated=%s", retryRunID, recoveredRetryRunID)
	}
	retriedV3, err := store.ClaimNext(ctx, "worker-test")
	if err != nil || retriedV3 == nil || retriedV3.RunID != retryRunID || retriedV3.Version != 3 {
		t.Fatalf("ClaimNext retried v3: run=%+v err=%v", retriedV3, err)
	}
	if err := store.StoreChunks(ctx, *retriedV3, []knowledge.IndexedChunk{{
		Chunk:   knowledge.Chunk{Index: 0, PageNumber: 3, Text: "재시도된 보안 정책"},
		Snippet: "재시도된 보안 정책", Embedding: vector,
	}}); err != nil {
		t.Fatalf("StoreChunks retried v3: %v", err)
	}
	if err := store.Finalize(ctx, *retriedV3, 3, 1); err != nil {
		t.Fatalf("Finalize retried v3: %v", err)
	}
	hits, err = store.Search(ctx, profile, vector, 5, "https://docs.example.test")
	if err != nil || len(hits) != 1 || hits[0].DocumentVersion != 3 {
		t.Fatalf("search retried v3: hits=%+v err=%v", hits, err)
	}

	duplicateName, err := knowledge.NewDocumentName("  보안   정책  ")
	if err != nil {
		t.Fatalf("NewDocumentName duplicate: %v", err)
	}
	duplicate, err := store.Register(ctx, knowledge.RegisterCommand{
		RequestKey:       "request-register-separate-same-name",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     duplicateName,
		ContentSHA256:    "9ff686cd2bf183215f3bcbfa3a7bf13ac1398ba4c11537d635962f1455e7f4b7",
		ByteSize:         4567,
		OriginalFileName: "separate-security-policy.pdf",
		StorageKey:       "9f/f6/9ff686cd.pdf",
	})
	if err != nil {
		t.Fatalf("Register separate Document with matching name: %v", err)
	}
	if duplicate.DocumentID == first.DocumentID {
		t.Fatal("separate same-name registration reused the existing Document ID")
	}
	matches, total, err := store.FindDocumentsByNormalizedName(ctx, duplicateName.Normalized, 1)
	if err != nil {
		t.Fatalf("FindDocumentsByNormalizedName: %v", err)
	}
	if total != 2 || len(matches) != 1 {
		t.Fatalf("same-name matches total=%d returned=%d, want total=2 returned=1", total, len(matches))
	}
}

func resetStoreFixtures(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		TRUNCATE TABLE syncbase.document,syncbase.processing_run,
			syncbase.document_version,syncbase.upload_request,syncbase.processing_checkpoint,
			syncbase.search_chunk,syncbase.change_log,syncbase.processing_step_attempt
			RESTART IDENTITY CASCADE;
		UPDATE syncbase.queue_control SET next_fencing_token=1 WHERE singleton=true`); err != nil {
		t.Fatalf("reset store fixtures: %v", err)
	}
}
