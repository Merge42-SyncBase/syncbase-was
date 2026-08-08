package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/jackc/pgx/v5"
)

func TestVerifyWritableClaimLocksTheRunUntilTheWriteTransactionEnds(t *testing.T) {
	databaseURL := os.Getenv("SYNCBASE_TEST_DB_URL")
	if databaseURL == "" {
		t.Skip("SYNCBASE_TEST_DB_URL is not set")
	}
	ctx := context.Background()
	pool, err := Open(ctx, databaseURL)
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
	if err := Migrate(ctx, pool, profile, canonical); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		TRUNCATE TABLE syncbase.document,syncbase.processing_run,
			syncbase.document_version,syncbase.upload_request,syncbase.processing_checkpoint,
			syncbase.search_chunk,syncbase.change_log,syncbase.processing_step_attempt
			RESTART IDENTITY CASCADE;
		UPDATE syncbase.queue_control SET next_fencing_token=1 WHERE singleton=true`); err != nil {
		t.Fatalf("reset fixtures: %v", err)
	}
	name, err := knowledge.NewDocumentName("locked fence contract")
	if err != nil {
		t.Fatalf("NewDocumentName: %v", err)
	}
	store := NewStore(pool)
	_, err = store.Register(ctx, knowledge.RegisterCommand{
		RequestKey:       "locked-fence-v1",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     name,
		ContentSHA256:    "3fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ByteSize:         1234,
		OriginalFileName: "locked-fence.pdf",
		StorageKey:       "3f/e7/3fe7058f.pdf",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	claimed, err := store.ClaimNext(ctx, "worker-lock-holder")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNext: run=%+v err=%v", claimed, err)
	}

	writeTx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin write transaction: %v", err)
	}
	defer func() { _ = writeTx.Rollback(ctx) }()
	if err := verifyWritableClaim(ctx, writeTx, *claimed); err != nil {
		t.Fatalf("verifyWritableClaim: %v", err)
	}

	contender, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin contender transaction: %v", err)
	}
	defer func() { _ = contender.Rollback(ctx) }()
	if _, err := contender.Exec(ctx, "SET LOCAL lock_timeout='100ms'"); err != nil {
		t.Fatalf("set contender lock timeout: %v", err)
	}
	if _, err := contender.Exec(ctx, `
		UPDATE syncbase.processing_run
		SET updated_at=clock_timestamp()
		WHERE id=$1`, claimed.RunID); err == nil {
		t.Fatal("contending run update succeeded while the fenced write held its row lock")
	}
}
