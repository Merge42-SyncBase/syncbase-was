// Package postgres implements SyncBase persistence against PostgreSQL/OpenSQL-compatible SQL.
package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

const maxLiveRuns = 21

const chunkInsertBatchSize = 64

// Store implements document, processing, and search persistence in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a PostgreSQL adapter over an initialized connection pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ready reports whether PostgreSQL accepts a request within the caller's context.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping document repository: %w", err)
	}
	return nil
}

// Register atomically creates an idempotent document version and processing run.
func (s *Store) Register(ctx context.Context, command knowledge.RegisterCommand) (knowledge.Registration, error) {
	if err := validateRegister(command); err != nil {
		return knowledge.Registration{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return knowledge.Registration{}, fmt.Errorf("begin registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT next_fencing_token FROM syncbase.queue_control WHERE singleton = true FOR UPDATE"); err != nil {
		return knowledge.Registration{}, fmt.Errorf("lock queue: %w", err)
	}
	if existing, found, err := findUpload(ctx, tx, command.RequestKey); err != nil {
		return knowledge.Registration{}, err
	} else if found {
		if existing.operation != string(command.Operation) || existing.contentSHA256 != command.ContentSHA256 ||
			existing.byteSize != command.ByteSize || !sameTarget(existing.targetDocumentID, command.TargetDocumentID) {
			return knowledge.Registration{}, knowledge.ErrIdempotencyConflict
		}
		existing.registration.Recovered = true
		return existing.registration, nil
	}
	var live int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM syncbase.processing_run WHERE status IN ('QUEUED','RUNNING')").Scan(&live); err != nil {
		return knowledge.Registration{}, fmt.Errorf("count live runs: %w", err)
	}
	if live >= maxLiveRuns {
		return knowledge.Registration{}, knowledge.ErrQueueFull
	}
	var profile string
	if err := tx.QueryRow(ctx, "SELECT fingerprint FROM syncbase.processing_profile WHERE active = true").Scan(&profile); err != nil {
		return knowledge.Registration{}, fmt.Errorf("load active profile: %w", err)
	}

	documentID := uuid.New()
	version := 1
	if command.Operation == knowledge.RegisterNewDocument {
		_, err = tx.Exec(ctx, `
			INSERT INTO syncbase.document(id, display_name, normalized_name, next_version)
			VALUES ($1, $2, $3, 2)`, documentID, command.DocumentName.Display, command.DocumentName.Normalized)
	} else {
		if command.TargetDocumentID == nil {
			return knowledge.Registration{}, knowledge.ErrInvalidArgument
		}
		documentID = *command.TargetDocumentID
		if err = tx.QueryRow(ctx, "SELECT next_version FROM syncbase.document WHERE id=$1 FOR UPDATE", documentID).Scan(&version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return knowledge.Registration{}, knowledge.ErrNotFound
			}
			return knowledge.Registration{}, fmt.Errorf("lock document: %w", err)
		}
		_, err = tx.Exec(ctx, "UPDATE syncbase.document SET next_version=next_version+1, updated_at=clock_timestamp() WHERE id=$1", documentID)
	}
	if err != nil {
		return knowledge.Registration{}, fmt.Errorf("write document: %w", err)
	}
	versionID := uuid.New()
	runID := uuid.New()
	correlationID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO syncbase.document_version(
			id, document_id, version_number, content_sha256, byte_size,
			original_file_name, storage_key, profile_fingerprint, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'QUEUED')`,
		versionID, documentID, version, command.ContentSHA256, command.ByteSize,
		command.OriginalFileName, command.StorageKey, profile,
	); err != nil {
		return knowledge.Registration{}, fmt.Errorf("write document version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO syncbase.processing_run(id, version_id, status, stage, correlation_id)
		VALUES ($1,$2,'QUEUED','METADATA',$3)`, runID, versionID, correlationID); err != nil {
		return knowledge.Registration{}, fmt.Errorf("write processing run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO syncbase.upload_request(
			request_key, operation, target_document_id, content_sha256, byte_size,
			status, document_id, version_id, processing_run_id
		) VALUES ($1,$2,$3,$4,$5,'ACCEPTED',$6,$7,$8)`,
		command.RequestKey, command.Operation, command.TargetDocumentID, command.ContentSHA256,
		command.ByteSize, documentID, versionID, runID,
	); err != nil {
		return knowledge.Registration{}, fmt.Errorf("write upload request: %w", err)
	}
	if err := appendChange(ctx, tx, documentID, versionID, runID, "VERSION_REGISTERED"); err != nil {
		return knowledge.Registration{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.Registration{}, fmt.Errorf("commit registration: %w", err)
	}
	return knowledge.Registration{
		DocumentID: documentID,
		VersionID:  versionID,
		Version:    version,
		RunID:      runID,
		Status:     knowledge.VersionQueued,
	}, nil
}

// ListDocuments returns a deterministic, bounded page of document summaries.
func (s *Store) ListDocuments(ctx context.Context, limit, offset int) ([]knowledge.DocumentSummary, error) {
	if limit < 1 || limit > 100 || offset < 0 {
		return nil, knowledge.ErrInvalidArgument
	}
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.display_name, active.version_number,
		       latest.version_number, latest.status, d.updated_at
		FROM syncbase.document d
		LEFT JOIN syncbase.document_version active ON active.id=d.active_version_id
		LEFT JOIN LATERAL (
			SELECT version_number,status FROM syncbase.document_version
			WHERE document_id=d.id ORDER BY version_number DESC LIMIT 1
		) latest ON true
		ORDER BY d.updated_at DESC,d.id ASC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()
	result := make([]knowledge.DocumentSummary, 0)
	for rows.Next() {
		var item knowledge.DocumentSummary
		var active pgtype.Int4
		if err := rows.Scan(&item.ID, &item.Name, &active, &item.LatestVersion, &item.LatestStatus, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		if active.Valid {
			value := int(active.Int32)
			item.ActiveVersion = &value
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate documents: %w", err)
	}
	return result, nil
}

// GetDocument returns one document and its newest-first version history.
func (s *Store) GetDocument(ctx context.Context, documentID uuid.UUID) (knowledge.DocumentDetails, error) {
	if documentID == uuid.Nil {
		return knowledge.DocumentDetails{}, knowledge.ErrInvalidArgument
	}
	details := knowledge.DocumentDetails{ID: documentID}
	var active pgtype.Int4
	if err := s.pool.QueryRow(ctx, `
		SELECT d.display_name, active.version_number
		FROM syncbase.document d
		LEFT JOIN syncbase.document_version active ON active.id=d.active_version_id
		WHERE d.id=$1`, documentID).Scan(&details.Name, &active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return knowledge.DocumentDetails{}, knowledge.ErrNotFound
		}
		return knowledge.DocumentDetails{}, fmt.Errorf("load document: %w", err)
	}
	if active.Valid {
		value := int(active.Int32)
		details.ActiveVersion = &value
	}
	rows, err := s.pool.Query(ctx, `
		SELECT v.id, v.version_number, v.status,
		       COALESCE(r.stage,'METADATA'), COALESCE(r.id,'00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(r.activation_outcome,'NOT_ATTEMPTED'), COALESCE(r.error_code,''),
		       COALESCE(r.correlation_id,''), COALESCE(r.automatic_attempts,0),
		       r.next_automatic_retry_at,
		       CASE WHEN r.status='QUEUED' OR (r.status='RUNNING' AND r.lease_owner IS NULL) THEN
		         1 + (SELECT count(*)::integer FROM syncbase.processing_run q
		              WHERE (q.status='QUEUED' OR (q.status='RUNNING' AND q.lease_owner IS NULL))
		                AND (COALESCE(q.next_automatic_retry_at,q.queued_at),q.id)
		                    < (COALESCE(r.next_automatic_retry_at,r.queued_at),r.id))
		       ELSE 0 END,
		       COALESCE(v.page_count,0), v.created_at, v.updated_at
		FROM syncbase.document_version v
		LEFT JOIN LATERAL (
			SELECT * FROM syncbase.processing_run
			WHERE version_id=v.id ORDER BY queued_at DESC,id DESC LIMIT 1
		) r ON true
		WHERE v.document_id=$1
		ORDER BY v.version_number DESC`, documentID)
	if err != nil {
		return knowledge.DocumentDetails{}, fmt.Errorf("load document versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version knowledge.VersionView
		if err := rows.Scan(
			&version.ID, &version.VersionNumber, &version.Status, &version.Stage,
			&version.RunID, &version.ActivationOutcome, &version.ErrorCode,
			&version.CorrelationID, &version.AutomaticAttempts, &version.NextAutomaticRetryAt,
			&version.QueuePosition, &version.PageCount, &version.CreatedAt, &version.UpdatedAt,
		); err != nil {
			return knowledge.DocumentDetails{}, fmt.Errorf("scan document version: %w", err)
		}
		version.Active = details.ActiveVersion != nil && *details.ActiveVersion == version.VersionNumber
		version.ManualRetryAllowed = version.Status == knowledge.VersionFailed &&
			version.ErrorCode == "TRANSIENT_EXHAUSTED"
		details.Versions = append(details.Versions, version)
	}
	if err := rows.Err(); err != nil {
		return knowledge.DocumentDetails{}, fmt.Errorf("iterate document versions: %w", err)
	}
	return details, nil
}

// GetSource returns the immutable original metadata for an exact version.
func (s *Store) GetSource(
	ctx context.Context,
	documentID uuid.UUID,
	versionNumber int,
) (knowledge.SourceDocument, error) {
	if documentID == uuid.Nil || versionNumber < 1 {
		return knowledge.SourceDocument{}, knowledge.ErrInvalidArgument
	}
	var source knowledge.SourceDocument
	err := s.pool.QueryRow(ctx, `
		SELECT d.id,d.display_name,v.id,v.version_number,v.storage_key,COALESCE(v.page_count,0)
		FROM syncbase.document d
		JOIN syncbase.document_version v ON v.document_id=d.id
		WHERE d.id=$1 AND v.version_number=$2`, documentID, versionNumber).Scan(
		&source.DocumentID, &source.Name, &source.VersionID, &source.Version,
		&source.StorageKey, &source.PageCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.SourceDocument{}, knowledge.ErrNotFound
	}
	if err != nil {
		return knowledge.SourceDocument{}, fmt.Errorf("load source document: %w", err)
	}
	return source, nil
}

// RecoverRegistration resolves the durable state of an idempotent upload key.
func (s *Store) RecoverRegistration(ctx context.Context, requestKey string) (knowledge.UploadRecovery, error) {
	if requestKey == "" || len(requestKey) > 200 {
		return knowledge.UploadRecovery{}, knowledge.ErrInvalidArgument
	}
	var uploadStatus string
	var expired bool
	var documentID, versionID, runID pgtype.UUID
	var versionNumber pgtype.Int4
	var versionStatus pgtype.Text
	err := s.pool.QueryRow(ctx, `
		SELECT u.status,u.expires_at <= clock_timestamp(),u.document_id,u.version_id,
		       v.version_number,u.processing_run_id,v.status
		FROM syncbase.upload_request u
		LEFT JOIN syncbase.document_version v ON v.id=u.version_id
		WHERE u.request_key=$1`, requestKey).Scan(
		&uploadStatus, &expired, &documentID, &versionID, &versionNumber, &runID, &versionStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.UploadRecovery{State: knowledge.UploadNotCommitted}, nil
	}
	if err != nil {
		return knowledge.UploadRecovery{}, fmt.Errorf("recover registration: %w", err)
	}
	if expired || uploadStatus == "EXPIRED" {
		return knowledge.UploadRecovery{State: knowledge.UploadExpired}, nil
	}
	if uploadStatus == "PENDING" {
		return knowledge.UploadRecovery{State: knowledge.UploadPending}, nil
	}
	if uploadStatus == "CONFLICT" {
		return knowledge.UploadRecovery{State: knowledge.UploadConflict}, nil
	}
	if uploadStatus != "ACCEPTED" || !documentID.Valid || !versionID.Valid ||
		!versionNumber.Valid || !runID.Valid || !versionStatus.Valid {
		return knowledge.UploadRecovery{}, fmt.Errorf("invalid accepted upload recovery state: %w", knowledge.ErrInvalidArgument)
	}
	registration := knowledge.Registration{
		DocumentID: uuid.UUID(documentID.Bytes), VersionID: uuid.UUID(versionID.Bytes),
		Version: int(versionNumber.Int32), RunID: uuid.UUID(runID.Bytes),
		Status: knowledge.VersionStatus(versionStatus.String), Recovered: true,
	}
	return knowledge.UploadRecovery{State: knowledge.UploadAccepted, Registration: registration}, nil
}

// StorageKeyReferenced reports whether a committed version references an object.
func (s *Store) StorageKeyReferenced(ctx context.Context, storageKey string) (bool, error) {
	if strings.TrimSpace(storageKey) == "" {
		return false, knowledge.ErrInvalidArgument
	}
	var referenced bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM syncbase.document_version WHERE storage_key=$1
	)`, storageKey).Scan(&referenced); err != nil {
		return false, fmt.Errorf("check storage reference: %w", err)
	}
	return referenced, nil
}

// ClaimNext exclusively leases the oldest queued processing run. The queue is
// deliberately single-flight for the P0 release; fencing tokens make an old
// worker harmless after its lease expires and the run is reclaimed.
func (s *Store) ClaimNext(ctx context.Context, workerID string) (*knowledge.ClaimedRun, error) {
	if strings.TrimSpace(workerID) == "" || len(workerID) > 200 {
		return nil, knowledge.ErrInvalidArgument
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT next_fencing_token FROM syncbase.queue_control WHERE singleton=true FOR UPDATE"); err != nil {
		return nil, fmt.Errorf("lock queue: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH expired AS MATERIALIZED (
			SELECT id,fencing_token
			FROM syncbase.processing_run
			WHERE status='RUNNING' AND lease_until <= clock_timestamp()
			FOR UPDATE
		), superseded_attempts AS (
			UPDATE syncbase.processing_step_attempt attempt
			SET outcome='SUPERSEDED',finished_at=clock_timestamp(),updated_at=clock_timestamp()
			FROM expired
			WHERE attempt.run_id=expired.id
			  AND attempt.fencing_token=expired.fencing_token
			  AND attempt.outcome='RUNNING'
		)
		UPDATE syncbase.processing_run run
		SET status='QUEUED', fencing_token=NULL, lease_owner=NULL, lease_until=NULL,
		    automatic_attempts=GREATEST(automatic_attempts-1,0),
		    next_automatic_retry_at=NULL, queued_at=clock_timestamp(), updated_at=clock_timestamp()
		FROM expired
		WHERE run.id=expired.id`); err != nil {
		return nil, fmt.Errorf("reclaim expired runs: %w", err)
	}
	var running bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM syncbase.processing_run
		WHERE status='RUNNING' AND lease_owner IS NOT NULL AND lease_until > clock_timestamp()
	)`).Scan(&running); err != nil {
		return nil, fmt.Errorf("check running work: %w", err)
	}
	if running {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty claim: %w", err)
		}
		return nil, nil
	}

	claimed := knowledge.ClaimedRun{}
	err = tx.QueryRow(ctx, `
		SELECT r.id, v.document_id, v.id, v.version_number, v.storage_key,
		       v.content_sha256, v.profile_fingerprint
		FROM syncbase.processing_run r
		JOIN syncbase.document_version v ON v.id=r.version_id
		WHERE r.status='QUEUED'
		   OR (r.status='RUNNING' AND r.lease_owner IS NULL
		       AND r.next_automatic_retry_at <= clock_timestamp())
		ORDER BY COALESCE(r.next_automatic_retry_at,r.queued_at), r.id
		LIMIT 1
		FOR UPDATE OF r SKIP LOCKED`).Scan(
		&claimed.RunID, &claimed.DocumentID, &claimed.VersionID, &claimed.Version,
		&claimed.StorageKey, &claimed.ContentSHA256, &claimed.ProfileFingerprint,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit empty queue: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select queued run: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		UPDATE syncbase.queue_control
		SET next_fencing_token=next_fencing_token+1
		WHERE singleton=true
		RETURNING next_fencing_token-1`).Scan(&claimed.Fence); err != nil {
		return nil, fmt.Errorf("allocate fencing token: %w", err)
	}
	err = tx.QueryRow(ctx, `
		UPDATE syncbase.processing_run
		SET status='RUNNING', stage='METADATA', fencing_token=$2, lease_owner=$3,
		    lease_until=clock_timestamp()+interval '30 seconds',
		    automatic_attempts=automatic_attempts+1, next_automatic_retry_at=NULL,
		    started_at=COALESCE(started_at,clock_timestamp()), updated_at=clock_timestamp()
		WHERE id=$1 AND automatic_attempts < 3
		  AND (status='QUEUED' OR (status='RUNNING' AND lease_owner IS NULL))
		RETURNING automatic_attempts`, claimed.RunID, claimed.Fence, workerID).Scan(&claimed.AutomaticAttempt)
	if err != nil {
		return nil, fmt.Errorf("lease queued run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE syncbase.document_version SET status='PROCESSING', updated_at=clock_timestamp()
		WHERE id=$1 AND status='QUEUED'`, claimed.VersionID); err != nil {
		return nil, fmt.Errorf("mark version processing: %w", err)
	}
	if err := recordAttemptOutcome(ctx, tx, claimed, knowledge.StageMetadata, "RUNNING", ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &claimed, nil
}

// SetStage advances the observable processing stage when the caller still owns
// the current lease.
func (s *Store) SetStage(ctx context.Context, runID uuid.UUID, fence int64, stage knowledge.Stage) error {
	if runID == uuid.Nil || fence < 1 || !validStage(stage) {
		return knowledge.ErrInvalidArgument
	}
	result, err := s.pool.Exec(ctx, `
		WITH current AS MATERIALIZED (
			SELECT id,automatic_attempts,fencing_token,stage
			FROM syncbase.processing_run
			WHERE id=$1 AND fencing_token=$2 AND status='RUNNING'
			  AND lease_owner IS NOT NULL AND lease_until > clock_timestamp()
			FOR UPDATE
		), completed_previous AS (
			UPDATE syncbase.processing_step_attempt attempt
			SET outcome='SUCCEEDED',finished_at=clock_timestamp(),updated_at=clock_timestamp()
			FROM current
			WHERE attempt.run_id=current.id
			  AND attempt.automatic_attempt=current.automatic_attempts
			  AND attempt.stage=current.stage
			  AND current.stage<>$3
			  AND attempt.outcome='RUNNING'
			RETURNING attempt.run_id
		), claimed AS (
			UPDATE syncbase.processing_run run
			SET stage=$3, lease_until=clock_timestamp()+interval '30 seconds', updated_at=clock_timestamp()
			FROM current
			WHERE run.id=current.id AND run.fencing_token=$2 AND run.status='RUNNING'
			RETURNING run.id,run.automatic_attempts,run.fencing_token
		)
		INSERT INTO syncbase.processing_step_attempt(
			run_id,automatic_attempt,stage,fencing_token,outcome
		)
		SELECT id,automatic_attempts,$3,fencing_token,'RUNNING' FROM claimed
		ON CONFLICT (run_id,fencing_token,stage) DO UPDATE SET
			fencing_token=EXCLUDED.fencing_token,outcome='RUNNING',error_code=NULL,
			finished_at=NULL,updated_at=clock_timestamp()`, runID, fence, stage)
	if err != nil {
		return fmt.Errorf("set processing stage: %w", err)
	}
	if result.RowsAffected() != 1 {
		return knowledge.ErrStaleFence
	}
	return nil
}

// LoadCheckpoint returns only an artifact whose input chain matches the
// currently fenced run. The artifact bytes and output digest are verified by
// the worker before use.
func (s *Store) LoadCheckpoint(
	ctx context.Context,
	claimed knowledge.ClaimedRun,
	stage knowledge.Stage,
	inputSHA256 string,
) (*knowledge.Checkpoint, error) {
	if err := validateClaimed(claimed); err != nil || !validStage(stage) || !validSHA256(inputSHA256) {
		return nil, knowledge.ErrInvalidArgument
	}
	checkpoint := knowledge.Checkpoint{RunID: claimed.RunID, Stage: stage}
	err := s.pool.QueryRow(ctx, `
		SELECT c.input_sha256,c.output_sha256,c.format_version,c.artifact_key,
		       c.artifact_size,c.fencing_token,c.completed_at
		FROM syncbase.processing_checkpoint c
		JOIN syncbase.processing_run r ON r.id=c.run_id
		WHERE c.run_id=$1 AND c.stage=$2 AND c.input_sha256=$3
		  AND r.fencing_token=$4 AND r.status='RUNNING'`,
		claimed.RunID, stage, inputSHA256, claimed.Fence).Scan(
		&checkpoint.InputSHA256, &checkpoint.OutputSHA256, &checkpoint.FormatVersion,
		&checkpoint.ArtifactKey, &checkpoint.ArtifactSize, &checkpoint.FencingToken,
		&checkpoint.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load processing checkpoint: %w", err)
	}
	return &checkpoint, nil
}

// SaveCheckpoint atomically attaches a content-addressed artifact to the
// currently fenced run. Reclaims may replace an older fence's checkpoint.
func (s *Store) SaveCheckpoint(
	ctx context.Context,
	claimed knowledge.ClaimedRun,
	checkpoint knowledge.Checkpoint,
) error {
	if err := validateClaimed(claimed); err != nil || checkpoint.RunID != claimed.RunID ||
		!validStage(checkpoint.Stage) || !validSHA256(checkpoint.InputSHA256) ||
		!validSHA256(checkpoint.OutputSHA256) || strings.TrimSpace(checkpoint.FormatVersion) == "" ||
		strings.TrimSpace(checkpoint.ArtifactKey) == "" || checkpoint.ArtifactSize < 1 ||
		checkpoint.FencingToken != claimed.Fence {
		return knowledge.ErrInvalidArgument
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin checkpoint save: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyWritableClaim(ctx, tx, claimed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO syncbase.processing_checkpoint(
			run_id,stage,input_sha256,output_sha256,format_version,
			artifact_key,artifact_size,fencing_token
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (run_id,stage) DO UPDATE SET
			input_sha256=EXCLUDED.input_sha256,
			output_sha256=EXCLUDED.output_sha256,
			format_version=EXCLUDED.format_version,
			artifact_key=EXCLUDED.artifact_key,
			artifact_size=EXCLUDED.artifact_size,
			fencing_token=EXCLUDED.fencing_token,
			completed_at=clock_timestamp()`,
		checkpoint.RunID, checkpoint.Stage, checkpoint.InputSHA256,
		checkpoint.OutputSHA256, checkpoint.FormatVersion, checkpoint.ArtifactKey,
		checkpoint.ArtifactSize, checkpoint.FencingToken); err != nil {
		return fmt.Errorf("upsert processing checkpoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit checkpoint: %w", err)
	}
	return nil
}

// Heartbeat extends the lease only for its current fenced owner.
func (s *Store) Heartbeat(ctx context.Context, runID uuid.UUID, fence int64, workerID string) error {
	if runID == uuid.Nil || fence < 1 || strings.TrimSpace(workerID) == "" {
		return knowledge.ErrInvalidArgument
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE syncbase.processing_run
		SET lease_until=clock_timestamp()+interval '30 seconds', updated_at=clock_timestamp()
		WHERE id=$1 AND fencing_token=$2 AND lease_owner=$3 AND status='RUNNING'
		  AND lease_until > clock_timestamp()`,
		runID, fence, workerID)
	if err != nil {
		return fmt.Errorf("renew processing lease: %w", err)
	}
	if result.RowsAffected() != 1 {
		return knowledge.ErrStaleFence
	}
	return nil
}

// VerifyProfile rejects a runtime whose processing profile differs from the database.
func (s *Store) VerifyProfile(ctx context.Context, profile knowledge.Profile) error {
	var fingerprint, parserID, chunkerID, embeddingModelID, distance, provider string
	var dimension, chunkSize, chunkOverlap int
	var minimumScore float64
	if err := s.pool.QueryRow(ctx, `
		SELECT fingerprint, parser_id, chunker_id, embedding_model_id, vector_dimension,
		       distance, minimum_score, provider, chunk_size_tokens, chunk_overlap_tokens
		FROM syncbase.processing_profile WHERE active=true`).Scan(
		&fingerprint, &parserID, &chunkerID, &embeddingModelID, &dimension,
		&distance, &minimumScore, &provider, &chunkSize, &chunkOverlap,
	); err != nil {
		return fmt.Errorf("load active profile: %w", err)
	}
	if fingerprint != profile.Fingerprint || parserID != profile.ParserID ||
		chunkerID != profile.ChunkerID || embeddingModelID != profile.EmbeddingModelID ||
		dimension != profile.VectorDimension || distance != profile.Distance ||
		minimumScore != profile.MinimumScore || provider != profile.Provider ||
		chunkSize != profile.ChunkSizeTokens || chunkOverlap != profile.ChunkOverlapTokens {
		return knowledge.ErrProfileMismatch
	}
	return nil
}

// StoreChunks atomically replaces staged chunks for one version and profile.
func (s *Store) StoreChunks(ctx context.Context, claimed knowledge.ClaimedRun, chunks []knowledge.IndexedChunk) error {
	if err := validateClaimed(claimed); err != nil {
		return err
	}
	seen := make(map[int]struct{}, len(chunks))
	for _, chunk := range chunks {
		if chunk.Index < 0 || chunk.PageNumber < 1 || chunk.PageNumber > knowledge.MaxPDFPages ||
			strings.TrimSpace(chunk.Text) == "" || len(chunk.Embedding) != knowledge.VectorDimension {
			return knowledge.ErrInvalidArgument
		}
		if _, exists := seen[chunk.Index]; exists {
			return knowledge.ErrInvalidArgument
		}
		seen[chunk.Index] = struct{}{}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin chunk store: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyWritableClaim(ctx, tx, claimed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM syncbase.search_chunk
		WHERE version_id=$1 AND profile_fingerprint=$2`, claimed.VersionID, claimed.ProfileFingerprint); err != nil {
		return fmt.Errorf("clear staged chunks: %w", err)
	}
	for start := 0; start < len(chunks); start += chunkInsertBatchSize {
		end := min(start+chunkInsertBatchSize, len(chunks))
		batch := &pgx.Batch{}
		for _, chunk := range chunks[start:end] {
			batch.Queue(`
				INSERT INTO syncbase.search_chunk(
					version_id, profile_fingerprint, chunk_index, page_number,
					full_text, snippet, embedding, staged_by_run_id
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				claimed.VersionID, claimed.ProfileFingerprint, chunk.Index, chunk.PageNumber,
				chunk.Text, chunk.Snippet, pgvector.NewVector(chunk.Embedding), claimed.RunID)
		}
		results := tx.SendBatch(ctx, batch)
		for index := start; index < end; index++ {
			if _, err := results.Exec(); err != nil {
				_ = results.Close()
				return fmt.Errorf("insert staged chunk %d: %w", chunks[index].Index, err)
			}
		}
		if err := results.Close(); err != nil {
			return fmt.Errorf("close staged chunk batch: %w", err)
		}
	}
	result, err := tx.Exec(ctx, `
			UPDATE syncbase.processing_run
			SET stage='STORE', lease_until=clock_timestamp()+interval '30 seconds', updated_at=clock_timestamp()
			WHERE id=$1 AND fencing_token=$2 AND status='RUNNING'`, claimed.RunID, claimed.Fence)
	if err != nil {
		return fmt.Errorf("record chunk stage: %w", err)
	}
	if result.RowsAffected() != 1 {
		return knowledge.ErrStaleFence
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chunks: %w", err)
	}
	return nil
}

// Finalize makes a successfully indexed version active. A lower version can
// never replace a higher version that is already active.
func (s *Store) Finalize(ctx context.Context, claimed knowledge.ClaimedRun, pageCount, chunkCount int) error {
	if err := validateClaimed(claimed); err != nil || pageCount < 1 || pageCount > knowledge.MaxPDFPages || chunkCount < 1 {
		return knowledge.ErrInvalidArgument
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin activation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyWritableClaim(ctx, tx, claimed); err != nil {
		return err
	}
	var stored int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM syncbase.search_chunk
		WHERE version_id=$1 AND profile_fingerprint=$2 AND staged_by_run_id=$3`,
		claimed.VersionID, claimed.ProfileFingerprint, claimed.RunID).Scan(&stored); err != nil {
		return fmt.Errorf("count staged chunks: %w", err)
	}
	if stored != chunkCount {
		return fmt.Errorf("stored chunk count %d does not match expected %d: %w", stored, chunkCount, knowledge.ErrInvalidArgument)
	}
	var activeVersionID pgtype.UUID
	var activeVersion int
	if err := tx.QueryRow(ctx, `
		SELECT d.active_version_id, COALESCE(active.version_number,0)
		FROM syncbase.document d
		LEFT JOIN syncbase.document_version active ON active.id=d.active_version_id
		WHERE d.id=$1 FOR UPDATE OF d`, claimed.DocumentID).Scan(&activeVersionID, &activeVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return knowledge.ErrNotFound
		}
		return fmt.Errorf("lock document for activation: %w", err)
	}
	if activeVersion > claimed.Version {
		if _, err := tx.Exec(ctx, `
			UPDATE syncbase.document_version
			SET status='SUPERSEDED', page_count=$2, updated_at=clock_timestamp()
			WHERE id=$1`, claimed.VersionID, pageCount); err != nil {
			return fmt.Errorf("supersede stale version: %w", err)
		}
		if err := recordAttemptOutcome(ctx, tx, claimed, knowledge.StageActivate, "SUPERSEDED", ""); err != nil {
			return err
		}
		if err := finishRun(ctx, tx, claimed, knowledge.RunSuperseded, "SKIPPED_SUPERSEDED"); err != nil {
			return err
		}
		if err := appendChange(ctx, tx, claimed.DocumentID, claimed.VersionID, claimed.RunID, "VERSION_SUPERSEDED"); err != nil {
			return err
		}
	} else {
		if activeVersionID.Valid && uuid.UUID(activeVersionID.Bytes) != claimed.VersionID {
			if _, err := tx.Exec(ctx, `
				UPDATE syncbase.document_version SET status='SUPERSEDED', updated_at=clock_timestamp()
				WHERE id=$1 AND status='ACTIVE'`, uuid.UUID(activeVersionID.Bytes)); err != nil {
				return fmt.Errorf("supersede previous active version: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE syncbase.document_version
			SET status='ACTIVE', page_count=$2, updated_at=clock_timestamp()
			WHERE id=$1`, claimed.VersionID, pageCount); err != nil {
			return fmt.Errorf("activate version: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE syncbase.document
			SET active_version_id=$2, updated_at=clock_timestamp()
			WHERE id=$1`, claimed.DocumentID, claimed.VersionID); err != nil {
			return fmt.Errorf("point document to active version: %w", err)
		}
		if err := recordAttemptOutcome(ctx, tx, claimed, knowledge.StageActivate, "SUCCEEDED", ""); err != nil {
			return err
		}
		if err := finishRun(ctx, tx, claimed, knowledge.RunSucceeded, "ACTIVATED"); err != nil {
			return err
		}
		if err := appendChange(ctx, tx, claimed.DocumentID, claimed.VersionID, claimed.RunID, "VERSION_ACTIVATED"); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit activation: %w", err)
	}
	return nil
}

// Fail terminates a run without exposing unsafe error details. The fencing
// predicate prevents an expired worker from overwriting a newer attempt.
func (s *Store) Fail(ctx context.Context, claimed knowledge.ClaimedRun, stage knowledge.Stage, code string) error {
	if err := validateClaimed(claimed); err != nil || !validStage(stage) ||
		strings.TrimSpace(code) == "" || len(code) > 100 {
		return knowledge.ErrInvalidArgument
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin failed run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := verifyWritableClaim(ctx, tx, claimed); err != nil {
		return err
	}
	if code == "TEMPORARILY_UNAVAILABLE" && claimed.AutomaticAttempt < 3 {
		delaySeconds := 1
		if claimed.AutomaticAttempt == 2 {
			delaySeconds = 5
		}
		result, err := tx.Exec(ctx, `
			UPDATE syncbase.processing_run
			SET stage=$3, error_code=$4, error_detail=NULL, fencing_token=NULL,
			    lease_owner=NULL, lease_until=NULL,
			    next_automatic_retry_at=clock_timestamp()+make_interval(secs => $5),
			    updated_at=clock_timestamp()
			WHERE id=$1 AND fencing_token=$2 AND status='RUNNING'`,
			claimed.RunID, claimed.Fence, stage, code, delaySeconds)
		if err != nil {
			return fmt.Errorf("schedule automatic retry: %w", err)
		}
		if result.RowsAffected() != 1 {
			return knowledge.ErrStaleFence
		}
		if err := recordAttemptOutcome(ctx, tx, claimed, stage, "RETRY_SCHEDULED", code); err != nil {
			return err
		}
		if err := appendChange(ctx, tx, claimed.DocumentID, claimed.VersionID, claimed.RunID, "AUTOMATIC_RETRY_SCHEDULED"); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit automatic retry: %w", err)
		}
		return nil
	}
	if code == "TEMPORARILY_UNAVAILABLE" {
		code = "TRANSIENT_EXHAUSTED"
	}
	result, err := tx.Exec(ctx, `
		UPDATE syncbase.processing_run
		SET status='FAILED', stage=$3, error_code=$4, error_detail=NULL,
		    lease_owner=NULL, lease_until=NULL, finished_at=clock_timestamp(), updated_at=clock_timestamp()
		WHERE id=$1 AND fencing_token=$2 AND status='RUNNING'`,
		claimed.RunID, claimed.Fence, stage, code)
	if err != nil {
		return fmt.Errorf("fail processing run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return knowledge.ErrStaleFence
	}
	if err := recordAttemptOutcome(ctx, tx, claimed, stage, "FAILED", code); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE syncbase.document_version SET status='FAILED', updated_at=clock_timestamp()
		WHERE id=$1 AND status='PROCESSING'`, claimed.VersionID); err != nil {
		return fmt.Errorf("fail document version: %w", err)
	}
	if err := appendChange(ctx, tx, claimed.DocumentID, claimed.VersionID, claimed.RunID, "VERSION_FAILED"); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit failed run: %w", err)
	}
	return nil
}

// Retry creates one idempotent child run for a failed version. It deliberately
// restarts the deterministic pipeline from metadata; content-addressed originals
// and staged-chunk replacement make that replay safe.
func (s *Store) Retry(ctx context.Context, parentRunID uuid.UUID, requestKey string) (uuid.UUID, error) {
	if parentRunID == uuid.Nil || strings.TrimSpace(requestKey) == "" || len(requestKey) > 200 {
		return uuid.Nil, knowledge.ErrInvalidArgument
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT next_fencing_token FROM syncbase.queue_control WHERE singleton=true FOR UPDATE"); err != nil {
		return uuid.Nil, fmt.Errorf("lock retry queue: %w", err)
	}
	var existingID, existingParent uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id,retry_of_run_id FROM syncbase.processing_run WHERE retry_request_key=$1`,
		requestKey).Scan(&existingID, &existingParent)
	if err == nil {
		if existingParent != parentRunID {
			return uuid.Nil, knowledge.ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, fmt.Errorf("commit recovered retry: %w", err)
		}
		return existingID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("load retry request: %w", err)
	}
	var runStatus knowledge.RunStatus
	var versionID, documentID uuid.UUID
	var versionStatus knowledge.VersionStatus
	var errorCode string
	var versionNumber, activeVersion int
	err = tx.QueryRow(ctx, `
		SELECT r.status,COALESCE(r.error_code,''),v.id,v.document_id,v.status,v.version_number,
		       COALESCE(active.version_number,0)
		FROM syncbase.processing_run r
		JOIN syncbase.document_version v ON v.id=r.version_id
		JOIN syncbase.document d ON d.id=v.document_id
		LEFT JOIN syncbase.document_version active ON active.id=d.active_version_id
		WHERE r.id=$1
		FOR UPDATE OF r,v,d`, parentRunID).Scan(
		&runStatus, &errorCode, &versionID, &documentID, &versionStatus, &versionNumber, &activeVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, knowledge.ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock failed run: %w", err)
	}
	if runStatus != knowledge.RunFailed || errorCode != "TRANSIENT_EXHAUSTED" ||
		versionStatus != knowledge.VersionFailed || activeVersion > versionNumber {
		return uuid.Nil, knowledge.ErrInvalidArgument
	}
	var live int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM syncbase.processing_run WHERE status IN ('QUEUED','RUNNING')`).Scan(&live); err != nil {
		return uuid.Nil, fmt.Errorf("count retry queue: %w", err)
	}
	if live >= maxLiveRuns {
		return uuid.Nil, knowledge.ErrQueueFull
	}
	childRunID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO syncbase.processing_run(
			id,version_id,retry_of_run_id,retry_request_key,status,stage,correlation_id
		) VALUES ($1,$2,$3,$4,'QUEUED','METADATA',$5)`,
		childRunID, versionID, parentRunID, requestKey, uuid.NewString()); err != nil {
		return uuid.Nil, fmt.Errorf("queue retry run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE syncbase.document_version SET status='QUEUED',updated_at=clock_timestamp()
		WHERE id=$1 AND status='FAILED'`, versionID); err != nil {
		return uuid.Nil, fmt.Errorf("requeue failed version: %w", err)
	}
	if err := appendChange(ctx, tx, documentID, versionID, childRunID, "VERSION_RETRY_QUEUED"); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit retry: %w", err)
	}
	return childRunID, nil
}

// Search returns chunks from active versions only, using the immutable active
// profile and the public, bounded score formula.
func (s *Store) Search(
	ctx context.Context,
	profile knowledge.Profile,
	query []float32,
	limit int,
	baseURL string,
) ([]knowledge.SearchHit, error) {
	if len(query) != knowledge.VectorDimension || limit < 1 || limit > 50 || strings.TrimSpace(baseURL) == "" {
		return nil, knowledge.ErrInvalidArgument
	}
	var fingerprint, parserID, chunkerID, embeddingModelID, distance, provider string
	var dimension, chunkSize, chunkOverlap int
	var minimumScore float64
	err := s.pool.QueryRow(ctx, `
		SELECT fingerprint, parser_id, chunker_id, embedding_model_id, vector_dimension,
		       distance, minimum_score, provider, chunk_size_tokens, chunk_overlap_tokens
		FROM syncbase.processing_profile WHERE active=true`).Scan(
		&fingerprint, &parserID, &chunkerID, &embeddingModelID, &dimension, &distance, &minimumScore,
		&provider, &chunkSize, &chunkOverlap,
	)
	if err != nil {
		return nil, databaseError("load search profile", err)
	}
	if fingerprint != profile.Fingerprint || parserID != profile.ParserID ||
		chunkerID != profile.ChunkerID || embeddingModelID != profile.EmbeddingModelID ||
		dimension != profile.VectorDimension || distance != profile.Distance || minimumScore != profile.MinimumScore ||
		provider != profile.Provider || chunkSize != profile.ChunkSizeTokens ||
		chunkOverlap != profile.ChunkOverlapTokens {
		return nil, knowledge.ErrProfileMismatch
	}
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.display_name, v.id, v.version_number, c.page_number, c.snippet,
		       (c.embedding <=> $1) AS cosine_distance
		FROM syncbase.search_chunk c
		JOIN syncbase.document_version v
		  ON v.id=c.version_id AND v.profile_fingerprint=c.profile_fingerprint
		JOIN syncbase.document d
		  ON d.id=v.document_id AND d.active_version_id=v.id
		WHERE c.profile_fingerprint=$2 AND v.status='ACTIVE'
		  AND (1.0 - ((c.embedding <=> $1) / 2.0)) >= $3
		ORDER BY cosine_distance ASC, d.id ASC, v.version_number DESC,
		         c.page_number ASC, c.chunk_index ASC
		LIMIT $4`, pgvector.NewVector(query), fingerprint, minimumScore, limit)
	if err != nil {
		return nil, databaseError("search active chunks", err)
	}
	defer rows.Close()
	hits := make([]knowledge.SearchHit, 0, limit)
	baseURL = strings.TrimRight(baseURL, "/")
	for rows.Next() {
		var hit knowledge.SearchHit
		var cosineDistance float64
		if err := rows.Scan(
			&hit.DocumentID, &hit.DocumentName, &hit.VersionID, &hit.DocumentVersion,
			&hit.PageNumber, &hit.Snippet, &cosineDistance,
		); err != nil {
			return nil, databaseError("scan search hit", err)
		}
		hit.Rank = len(hits) + 1
		hit.Score = knowledge.ScoreFromCosineDistance(cosineDistance)
		hit.SourceURL = fmt.Sprintf(
			"%s/sources/%s/versions/%d?page=%d",
			baseURL, hit.DocumentID, hit.DocumentVersion, hit.PageNumber,
		)
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError("iterate search hits", err)
	}
	return hits, nil
}

func verifyWritableClaim(ctx context.Context, tx pgx.Tx, claimed knowledge.ClaimedRun) error {
	var runID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT r.id
		FROM syncbase.processing_run r
		JOIN syncbase.document_version v ON v.id=r.version_id
		WHERE r.id=$1 AND r.fencing_token=$2 AND r.status='RUNNING'
		  AND r.lease_owner IS NOT NULL AND r.lease_until > clock_timestamp()
		  AND v.id=$3 AND v.document_id=$4 AND v.version_number=$5
		  AND v.profile_fingerprint=$6
		FOR UPDATE OF r`, claimed.RunID, claimed.Fence, claimed.VersionID, claimed.DocumentID,
		claimed.Version, claimed.ProfileFingerprint).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.ErrStaleFence
	}
	if err != nil {
		return fmt.Errorf("verify processing lease: %w", err)
	}
	return nil
}

func finishRun(
	ctx context.Context,
	tx pgx.Tx,
	claimed knowledge.ClaimedRun,
	status knowledge.RunStatus,
	outcome string,
) error {
	result, err := tx.Exec(ctx, `
		UPDATE syncbase.processing_run
		SET status=$3, stage='ACTIVATE', activation_outcome=$4,
		    lease_owner=NULL, lease_until=NULL, finished_at=clock_timestamp(), updated_at=clock_timestamp()
		WHERE id=$1 AND fencing_token=$2 AND status='RUNNING'`,
		claimed.RunID, claimed.Fence, status, outcome)
	if err != nil {
		return fmt.Errorf("finish processing run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return knowledge.ErrStaleFence
	}
	return nil
}

func validStage(stage knowledge.Stage) bool {
	for _, candidate := range knowledge.ProcessingStages() {
		if stage == candidate {
			return true
		}
	}
	return false
}

func validateClaimed(claimed knowledge.ClaimedRun) error {
	if claimed.RunID == uuid.Nil || claimed.DocumentID == uuid.Nil || claimed.VersionID == uuid.Nil ||
		claimed.Version < 1 || claimed.Fence < 1 || claimed.AutomaticAttempt < 1 ||
		claimed.AutomaticAttempt > 3 || claimed.StorageKey == "" ||
		len(claimed.ContentSHA256) != 64 || len(claimed.ProfileFingerprint) != 64 {
		return knowledge.ErrInvalidArgument
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type upload struct {
	operation        string
	targetDocumentID *uuid.UUID
	contentSHA256    string
	byteSize         int64
	registration     knowledge.Registration
}

func findUpload(ctx context.Context, tx pgx.Tx, requestKey string) (upload, bool, error) {
	var item upload
	var target pgtype.Text
	var status knowledge.VersionStatus
	err := tx.QueryRow(ctx, `
		SELECT u.operation, u.target_document_id::text, u.content_sha256, u.byte_size,
		       u.document_id, u.version_id, v.version_number, u.processing_run_id, v.status
		FROM syncbase.upload_request u
		JOIN syncbase.document_version v ON v.id=u.version_id
		WHERE u.request_key=$1 AND u.expires_at > clock_timestamp()`, requestKey).Scan(
		&item.operation, &target, &item.contentSHA256, &item.byteSize,
		&item.registration.DocumentID, &item.registration.VersionID, &item.registration.Version,
		&item.registration.RunID, &status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return upload{}, false, nil
	}
	if err != nil {
		return upload{}, false, fmt.Errorf("load upload request: %w", err)
	}
	if target.Valid {
		value, err := uuid.Parse(target.String)
		if err != nil {
			return upload{}, false, fmt.Errorf("decode upload target: %w", err)
		}
		item.targetDocumentID = &value
	}
	item.registration.Status = status
	return item, true, nil
}

func validateRegister(command knowledge.RegisterCommand) error {
	if command.RequestKey == "" || len(command.RequestKey) > 200 ||
		(command.Operation != knowledge.RegisterNewDocument && command.Operation != knowledge.RegisterNewVersion) ||
		len(command.ContentSHA256) != 64 || command.ByteSize < 1 || command.ByteSize > knowledge.MaxUploadBytes ||
		command.OriginalFileName == "" || command.StorageKey == "" {
		return knowledge.ErrInvalidArgument
	}
	if command.Operation == knowledge.RegisterNewDocument && command.TargetDocumentID != nil {
		return knowledge.ErrInvalidArgument
	}
	if command.Operation == knowledge.RegisterNewVersion && command.TargetDocumentID == nil {
		return knowledge.ErrInvalidArgument
	}
	return nil
}

func sameTarget(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func appendChange(
	ctx context.Context,
	tx pgx.Tx,
	documentID, versionID, runID uuid.UUID,
	eventType string,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO syncbase.change_log(document_id,version_id,run_id,event_type)
		VALUES ($1,$2,$3,$4)`, documentID, versionID, runID, eventType)
	if err != nil {
		return fmt.Errorf("append change log: %w", err)
	}
	return nil
}

func recordAttemptOutcome(
	ctx context.Context,
	tx pgx.Tx,
	claimed knowledge.ClaimedRun,
	stage knowledge.Stage,
	outcome, errorCode string,
) error {
	finished := outcome != "RUNNING"
	_, err := tx.Exec(ctx, `
		INSERT INTO syncbase.processing_step_attempt(
			run_id,automatic_attempt,stage,fencing_token,outcome,error_code,finished_at
		) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),
			CASE WHEN $7 THEN clock_timestamp() ELSE NULL END)
		ON CONFLICT (run_id,fencing_token,stage) DO UPDATE SET
			fencing_token=EXCLUDED.fencing_token,outcome=EXCLUDED.outcome,
			error_code=EXCLUDED.error_code,finished_at=EXCLUDED.finished_at,
			updated_at=clock_timestamp()`, claimed.RunID, claimed.AutomaticAttempt,
		stage, claimed.Fence, outcome, errorCode, finished)
	if err != nil {
		return fmt.Errorf("record processing attempt: %w", err)
	}
	return nil
}
