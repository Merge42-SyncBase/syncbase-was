// Package knowledge defines SyncBase's document, version, processing, and search contracts.
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	// VectorDimension is the only supported embedding dimension for the active profile.
	VectorDimension = 384
	// MaxUploadBytes is the maximum accepted PDF size.
	MaxUploadBytes = 100 * 1024 * 1024
	// MaxPDFPages is the maximum accepted page count.
	MaxPDFPages = 500
)

var (
	// ErrInvalidArgument reports a caller contract violation.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrUnauthenticated reports missing or invalid credentials.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrTemporarilyUnavailable reports a retryable dependency failure.
	ErrTemporarilyUnavailable = errors.New("temporarily unavailable")
	// ErrProfileMismatch reports incompatible processing or search profiles.
	ErrProfileMismatch = errors.New("profile mismatch")
	// ErrNotFound reports a missing document or version.
	ErrNotFound = errors.New("not found")
	// ErrQueueFull reports bounded processing-queue exhaustion.
	ErrQueueFull = errors.New("queue full")
	// ErrIdempotencyConflict reports reuse of a key for different input.
	ErrIdempotencyConflict = errors.New("idempotency conflict")
	// ErrStaleFence reports work attempted by an expired processing owner.
	ErrStaleFence = errors.New("stale fence")
	// ErrInvalidPDF reports a PDF that violates the supported input contract.
	ErrInvalidPDF = errors.New("invalid PDF")
)

// DocumentName is the user-visible name and its comparison key.
type DocumentName struct {
	Display    string
	Normalized string
}

// NewDocumentName validates and normalizes a document name.
func NewDocumentName(value string) (DocumentName, error) {
	display := strings.TrimSpace(value)
	if utf8.RuneCountInString(display) < 1 || utf8.RuneCountInString(display) > 200 {
		return DocumentName{}, ErrInvalidArgument
	}
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return unicode.ToLower(r)
	}, display)
	normalized = strings.Join(strings.Fields(normalized), " ")
	return DocumentName{Display: display, Normalized: normalized}, nil
}

// VersionStatus is the durable lifecycle state of one document version.
type VersionStatus string

const (
	// VersionQueued is awaiting a processing lease.
	VersionQueued VersionStatus = "QUEUED"
	// VersionProcessing is owned by a live or recoverable processing run.
	VersionProcessing VersionStatus = "PROCESSING"
	// VersionActive is the only version exposed to search.
	VersionActive VersionStatus = "ACTIVE"
	// VersionFailed requires automatic or manual recovery.
	VersionFailed VersionStatus = "FAILED"
	// VersionSuperseded has been replaced by a newer active version.
	VersionSuperseded VersionStatus = "SUPERSEDED"
)

// RunStatus is the durable lifecycle state of one processing run.
type RunStatus string

const (
	// RunQueued is eligible for a processing lease.
	RunQueued RunStatus = "QUEUED"
	// RunRunning currently owns or awaits recovery of a lease.
	RunRunning RunStatus = "RUNNING"
	// RunSucceeded activated its version.
	RunSucceeded RunStatus = "SUCCEEDED"
	// RunFailed exhausted its permitted recovery path.
	RunFailed RunStatus = "FAILED"
	// RunSuperseded completed after a newer version was already active.
	RunSuperseded RunStatus = "SUPERSEDED"
)

// Stage identifies one checkpointed processing phase.
type Stage string

const (
	// StageMetadata initializes a claimed run.
	StageMetadata Stage = "METADATA"
	// StageParse extracts page-scoped text.
	StageParse Stage = "PARSE"
	// StageChunk creates page-bounded passages.
	StageChunk Stage = "CHUNK"
	// StageEmbed creates pinned-profile vectors.
	StageEmbed Stage = "EMBED"
	// StageStore persists staged search chunks.
	StageStore Stage = "STORE"
	// StageActivate atomically publishes the completed version.
	StageActivate Stage = "ACTIVATE"
)

var processingStages = [...]Stage{
	StageMetadata,
	StageParse,
	StageChunk,
	StageEmbed,
	StageStore,
	StageActivate,
}

// ProcessingStages returns the processing stages in execution order.
func ProcessingStages() []Stage {
	stages := make([]Stage, len(processingStages))
	copy(stages, processingStages[:])
	return stages
}

// Profile is the immutable parser, chunker, embedding, and ranking contract.
type Profile struct {
	Fingerprint      string
	ParserID         string
	ChunkerID        string
	EmbeddingModelID string
	ONNXRuntimeID    string
	VectorDimension  int
	Distance         string
	MinimumScore     float64
}

// NewProfile builds the immutable Go processing profile and canonical representation.
func NewProfile(modelSHA256, tokenizerSHA256, onnxRuntimeID string, minimumScore float64) (Profile, string, error) {
	if !isSHA256(modelSHA256) || !isSHA256(tokenizerSHA256) || math.IsNaN(minimumScore) ||
		math.IsInf(minimumScore, 0) || minimumScore < 0 || minimumScore > 1 ||
		onnxRuntimeID != "onnxruntime-1.26.0" {
		return Profile{}, "", ErrInvalidArgument
	}
	canonical := fmt.Sprintf(
		`{"chunker_id":"page-aware-recursive-v1","distance":"cosine",`+
			`"embedding_model_id":"intfloat/multilingual-e5-small",`+
			`"embedding_model_sha256":"%s","minimum_score":%.6f,`+
			`"onnx_runtime_id":"%s",`+
			`"parser_id":"pdfium-wasm-1.19.6","tokenizer_sha256":"%s",`+
			`"vector_dimension":384}`,
		modelSHA256,
		minimumScore,
		onnxRuntimeID,
		tokenizerSHA256,
	)
	digest := sha256.Sum256([]byte(canonical))
	return Profile{
		Fingerprint:      hex.EncodeToString(digest[:]),
		ParserID:         "pdfium-wasm-1.19.6",
		ChunkerID:        "page-aware-recursive-v1",
		EmbeddingModelID: "intfloat/multilingual-e5-small",
		ONNXRuntimeID:    onnxRuntimeID,
		VectorDimension:  VectorDimension,
		Distance:         "cosine",
		MinimumScore:     minimumScore,
	}, canonical, nil
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

// PageText is normalized text extracted from one PDF page.
type PageText struct {
	PageNumber int    `json:"page_number"`
	Text       string `json:"text"`
}

// Chunk is one page-bounded searchable passage.
type Chunk struct {
	Index      int    `json:"chunk_index"`
	PageNumber int    `json:"page_number"`
	Text       string `json:"text"`
}

// IndexedChunk adds a public snippet and embedding to a chunk.
type IndexedChunk struct {
	Chunk
	Snippet   string    `json:"snippet"`
	Embedding []float32 `json:"embedding"`
}

// DocumentSummary is the list projection for one logical document.
type DocumentSummary struct {
	ID            uuid.UUID
	Name          string
	ActiveVersion *int
	LatestVersion int
	LatestStatus  VersionStatus
	UpdatedAt     time.Time
}

// VersionView is the administrator projection for one document version.
type VersionView struct {
	ID                   uuid.UUID
	VersionNumber        int
	Status               VersionStatus
	Active               bool
	Stage                Stage
	RunID                uuid.UUID
	ActivationOutcome    string
	ErrorCode            string
	CorrelationID        string
	AutomaticAttempts    int
	NextAutomaticRetryAt *time.Time
	ManualRetryAllowed   bool
	QueuePosition        int
	PageCount            int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// DocumentDetails contains a logical document and newest-first version history.
type DocumentDetails struct {
	ID            uuid.UUID
	Name          string
	ActiveVersion *int
	Versions      []VersionView
}

// SourceDocument identifies the immutable original for one version.
type SourceDocument struct {
	DocumentID uuid.UUID
	Name       string
	VersionID  uuid.UUID
	Version    int
	StorageKey string
	PageCount  int
}

// Registration identifies the version and processing run created by an upload.
type Registration struct {
	DocumentID uuid.UUID
	VersionID  uuid.UUID
	Version    int
	RunID      uuid.UUID
	Status     VersionStatus
	Recovered  bool
}

// UploadRecoveryState describes the durable outcome of an idempotent upload key.
type UploadRecoveryState string

const (
	// UploadNotCommitted means no durable request exists for the key.
	UploadNotCommitted UploadRecoveryState = "not_committed"
	// UploadPending means registration has not reached a durable outcome.
	UploadPending UploadRecoveryState = "pending"
	// UploadAccepted means registration committed successfully.
	UploadAccepted UploadRecoveryState = "accepted"
	// UploadConflict means the key was reused for different input.
	UploadConflict UploadRecoveryState = "conflict"
	// UploadExpired means the recovery window has elapsed.
	UploadExpired UploadRecoveryState = "expired"
)

// UploadRecovery returns a recovery state and accepted registration when present.
type UploadRecovery struct {
	State        UploadRecoveryState
	Registration Registration
}

// RegistrationOperation distinguishes new documents from new versions.
type RegistrationOperation string

const (
	// RegisterNewDocument creates a logical document and version one.
	RegisterNewDocument RegistrationOperation = "NEW_DOCUMENT"
	// RegisterNewVersion appends the next version to an existing document.
	RegisterNewVersion RegistrationOperation = "NEW_VERSION"
)

// RegisterCommand is the validated persistence command for one uploaded PDF.
type RegisterCommand struct {
	RequestKey       string
	Operation        RegistrationOperation
	TargetDocumentID *uuid.UUID
	DocumentName     DocumentName
	ContentSHA256    string
	ByteSize         int64
	OriginalFileName string
	StorageKey       string
}

// ClaimedRun carries the immutable identity and fencing token for leased work.
type ClaimedRun struct {
	RunID              uuid.UUID
	DocumentID         uuid.UUID
	VersionID          uuid.UUID
	Version            int
	StorageKey         string
	ContentSHA256      string
	ProfileFingerprint string
	Fence              int64
	AutomaticAttempt   int
}

// Checkpoint identifies a content-verified intermediate pipeline artifact.
// InputSHA256 chains stages together so output from a different original or
// processing profile can never be resumed accidentally.
type Checkpoint struct {
	RunID         uuid.UUID
	Stage         Stage
	InputSHA256   string
	OutputSHA256  string
	FormatVersion string
	ArtifactKey   string
	ArtifactSize  int64
	FencingToken  int64
	CompletedAt   time.Time
}

// SearchHit is one ranked, page-grounded result from an active version.
type SearchHit struct {
	Rank            int       `json:"rank"`
	Score           float64   `json:"score"`
	DocumentID      uuid.UUID `json:"document_id"`
	DocumentName    string    `json:"document_name"`
	DocumentVersion int       `json:"document_version"`
	PageNumber      int       `json:"page_number"`
	Snippet         string    `json:"snippet"`
	SourceURL       string    `json:"source_url"`
}

// ScoreFromCosineDistance maps pgvector cosine distance to the stable public score.
func ScoreFromCosineDistance(distance float64) float64 {
	score := 1 - distance/2
	return math.Max(0, math.Min(1, score))
}
