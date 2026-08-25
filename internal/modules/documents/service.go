// Package documents implements document registration, recovery, and source access.
package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
)

const (
	cleanupTimeout    = 5 * time.Second
	maxNameMatchLimit = 10
)

type repository interface {
	ListDocuments(context.Context, int, int) ([]knowledge.DocumentSummary, error)
	FindDocumentsByNormalizedName(context.Context, string, int) ([]knowledge.DocumentSummary, int, error)
	GetDocument(context.Context, uuid.UUID) (knowledge.DocumentDetails, error)
	GetSource(context.Context, uuid.UUID, int) (knowledge.SourceDocument, error)
	RecoverRegistration(context.Context, string) (knowledge.UploadRecovery, error)
	ReserveRegistration(context.Context, knowledge.ReserveUploadCommand) (knowledge.UploadRecovery, error)
	ExpireRegistration(context.Context, knowledge.ReserveUploadCommand) error
	StorageKeyReferenced(context.Context, string) (bool, error)
	Register(context.Context, knowledge.RegisterCommand) (knowledge.Registration, error)
	Retry(context.Context, uuid.UUID, string) (uuid.UUID, error)
	Ready(context.Context) error
}

type originalStore interface {
	Put(context.Context, []byte) (string, error)
	Path(string) (string, error)
	Verify(context.Context, string, string) error
	Remove(context.Context, string) error
	Ready(context.Context) error
}

type parser interface {
	Parse(context.Context, []byte) ([]knowledge.PageText, error)
	Ready(context.Context) error
}

// Preflight describes a PDF that passed parser validation without persisting it.
type Preflight struct {
	FileName      string
	ByteSize      int
	PageCount     int
	SHA256        string
	SuggestedName string
}

// NameMatches describes existing Documents whose normalized display name is
// equal to a proposed name. Matches are guidance only and never block creation.
type NameMatches struct {
	NormalizedName string
	Total          int
	Documents      []knowledge.DocumentSummary
}

// RegisterCommand contains the user intent and uploaded PDF for one registration.
type RegisterCommand struct {
	RequestKey       string
	Operation        knowledge.RegistrationOperation
	TargetDocumentID *uuid.UUID
	DocumentName     string
	OriginalFileName string
	Content          []byte
}

// Source identifies an immutable document version and its confined local path.
type Source struct {
	Document knowledge.SourceDocument
	Path     string
}

// Service owns document registration and access policy across persistence adapters.
type Service struct {
	repository repository
	originals  originalStore
	parser     parser
	registerMu sync.Mutex
}

// New returns a document module backed by the provided persistence adapters.
func New(repository repository, originals originalStore, parser parser) (*Service, error) {
	if repository == nil || originals == nil || parser == nil {
		return nil, fmt.Errorf("configure documents: %w", knowledge.ErrInvalidArgument)
	}
	return &Service{repository: repository, originals: originals, parser: parser}, nil
}

// Preflight validates a PDF and returns stable metadata without storing it.
func (s *Service) Preflight(ctx context.Context, fileName string, content []byte) (Preflight, error) {
	fileName, pages, err := s.inspectPDF(ctx, fileName, content)
	if err != nil {
		return Preflight{}, err
	}
	digest := sha256.Sum256(content)
	return Preflight{
		FileName:      fileName,
		ByteSize:      len(content),
		PageCount:     len(pages),
		SHA256:        hex.EncodeToString(digest[:]),
		SuggestedName: suggestedName(fileName),
	}, nil
}

// Register validates and stores a PDF before atomically registering its version.
// If the database write fails, it removes the object only after proving that no
// committed version references the content-addressed key.
func (s *Service) Register(ctx context.Context, command RegisterCommand) (knowledge.Registration, error) {
	fileName, err := validatePDFEnvelope(command.OriginalFileName, command.Content)
	if err != nil {
		return knowledge.Registration{}, err
	}
	name := knowledge.DocumentName{}
	switch command.Operation {
	case knowledge.RegisterNewDocument:
		name, err = knowledge.NewDocumentName(command.DocumentName)
	case knowledge.RegisterNewVersion:
		if command.TargetDocumentID == nil || *command.TargetDocumentID == uuid.Nil {
			err = knowledge.ErrInvalidArgument
		}
	default:
		err = knowledge.ErrInvalidArgument
	}
	if err != nil {
		return knowledge.Registration{}, err
	}
	digest := sha256.Sum256(command.Content)
	contentSHA256 := hex.EncodeToString(digest[:])
	started := time.Now()
	recovery, err := s.repository.ReserveRegistration(ctx, knowledge.ReserveUploadCommand{
		RequestKey: command.RequestKey, Operation: command.Operation,
		TargetDocumentID: command.TargetDocumentID, ContentSHA256: contentSHA256,
		ByteSize: int64(len(command.Content)),
	})
	logRegistrationStage(ctx, "pending_reservation", started, err)
	if err != nil {
		return knowledge.Registration{}, err
	}
	if recovery.State == knowledge.UploadAccepted {
		recovery.Registration.Recovered = true
		return recovery.Registration, nil
	}
	if recovery.State != knowledge.UploadPending {
		return knowledge.Registration{}, knowledge.ErrIdempotencyConflict
	}

	started = time.Now()
	_, _, err = s.inspectPDF(ctx, fileName, command.Content)
	logRegistrationStage(ctx, "parse", started, err)
	if err != nil {
		if errors.Is(err, knowledge.ErrInvalidPDF) || errors.Is(err, knowledge.ErrInvalidArgument) {
			cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			expireErr := s.repository.ExpireRegistration(cleanupContext, knowledge.ReserveUploadCommand{
				RequestKey: command.RequestKey, Operation: command.Operation,
				TargetDocumentID: command.TargetDocumentID, ContentSHA256: contentSHA256,
				ByteSize: int64(len(command.Content)),
			})
			cancel()
			if expireErr != nil {
				return knowledge.Registration{}, errors.Join(err, fmt.Errorf("expire rejected upload reservation: %w", expireErr))
			}
		}
		return knowledge.Registration{}, err
	}

	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	started = time.Now()
	storageKey, err := s.originals.Put(ctx, command.Content)
	logRegistrationStage(ctx, "original_sync", started, err)
	if err != nil {
		return knowledge.Registration{}, fmt.Errorf("store original PDF: %w", err)
	}
	started = time.Now()
	registration, err := s.repository.Register(ctx, knowledge.RegisterCommand{
		RequestKey:       command.RequestKey,
		Operation:        command.Operation,
		TargetDocumentID: command.TargetDocumentID,
		DocumentName:     name,
		ContentSHA256:    contentSHA256,
		ByteSize:         int64(len(command.Content)),
		OriginalFileName: fileName,
		StorageKey:       storageKey,
	})
	logRegistrationStage(ctx, "transaction", started, err)
	if err == nil {
		return registration, nil
	}
	if cleanupErr := s.removeUncommittedOriginal(ctx, storageKey); cleanupErr != nil {
		return knowledge.Registration{}, errors.Join(err, cleanupErr)
	}
	return knowledge.Registration{}, err
}

// ListDocuments returns one bounded page of documents.
func (s *Service) ListDocuments(ctx context.Context, limit, offset int) ([]knowledge.DocumentSummary, error) {
	return s.repository.ListDocuments(ctx, limit, offset)
}

// FindNameMatches returns bounded, exact normalized-name matches so callers can
// distinguish creating a separate Document from adding a Version.
func (s *Service) FindNameMatches(ctx context.Context, value string, limit int) (NameMatches, error) {
	name, err := knowledge.NewDocumentName(value)
	if err != nil || limit < 1 || limit > maxNameMatchLimit {
		return NameMatches{}, knowledge.ErrInvalidArgument
	}
	documents, total, err := s.repository.FindDocumentsByNormalizedName(ctx, name.Normalized, limit)
	if err != nil {
		return NameMatches{}, err
	}
	return NameMatches{NormalizedName: name.Normalized, Total: total, Documents: documents}, nil
}

// GetDocument returns document details and version history.
func (s *Service) GetDocument(ctx context.Context, documentID uuid.UUID) (knowledge.DocumentDetails, error) {
	return s.repository.GetDocument(ctx, documentID)
}

// RecoverRegistration resolves an idempotent upload request after response loss.
func (s *Service) RecoverRegistration(ctx context.Context, requestKey string) (knowledge.UploadRecovery, error) {
	return s.repository.RecoverRegistration(ctx, requestKey)
}

// Retry queues one idempotent manual retry for an exhausted transient run.
func (s *Service) Retry(ctx context.Context, runID uuid.UUID, requestKey string) (uuid.UUID, error) {
	return s.repository.Retry(ctx, runID, requestKey)
}

// Source returns an immutable version and the confined path to its original PDF.
func (s *Service) Source(ctx context.Context, documentID uuid.UUID, version int) (Source, error) {
	document, err := s.repository.GetSource(ctx, documentID, version)
	if err != nil {
		return Source{}, err
	}
	if err := s.originals.Verify(ctx, document.StorageKey, document.ContentSHA256); err != nil {
		return Source{}, fmt.Errorf("verify source PDF: %v: %w", err, knowledge.ErrNotFound)
	}
	path, err := s.originals.Path(document.StorageKey)
	if err != nil {
		return Source{}, fmt.Errorf("resolve source PDF: %v: %w", err, knowledge.ErrNotFound)
	}
	return Source{Document: document, Path: path}, nil
}

// Ready reports whether every dependency required for document requests is available.
func (s *Service) Ready(ctx context.Context) error {
	if err := s.repository.Ready(ctx); err != nil {
		return fmt.Errorf("document repository readiness: %w", err)
	}
	if err := s.originals.Ready(ctx); err != nil {
		return fmt.Errorf("original store readiness: %w", err)
	}
	if err := s.parser.Ready(ctx); err != nil {
		return fmt.Errorf("PDF parser readiness: %w", err)
	}
	return nil
}

func (s *Service) inspectPDF(
	ctx context.Context,
	fileName string,
	content []byte,
) (string, []knowledge.PageText, error) {
	fileName, err := validatePDFEnvelope(fileName, content)
	if err != nil {
		return "", nil, err
	}
	pages, err := s.parser.Parse(ctx, content)
	if err != nil {
		return "", nil, err
	}
	if len(pages) < 1 || len(pages) > knowledge.MaxPDFPages {
		return "", nil, knowledge.ErrInvalidPDF
	}
	return fileName, pages, nil
}

func validatePDFEnvelope(fileName string, content []byte) (string, error) {
	fileName = filepath.Base(fileName)
	if fileName == "." || fileName == "" || utf8.RuneCountInString(fileName) > 255 {
		return "", fmt.Errorf("invalid PDF filename: %w", knowledge.ErrInvalidArgument)
	}
	if len(content) < 5 || len(content) > knowledge.MaxUploadBytes || string(content[:5]) != "%PDF-" {
		return "", fmt.Errorf("invalid PDF upload: %w", knowledge.ErrInvalidPDF)
	}
	return fileName, nil
}

func logRegistrationStage(ctx context.Context, stage string, started time.Time, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	slog.InfoContext(ctx, "document registration stage",
		"stage", stage,
		"duration_ms", time.Since(started).Milliseconds(),
		"outcome", outcome,
	)
}

func (s *Service) removeUncommittedOriginal(ctx context.Context, storageKey string) error {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	referenced, err := s.repository.StorageKeyReferenced(cleanupContext, storageKey)
	if err != nil {
		return fmt.Errorf("verify failed registration object: %w", err)
	}
	if referenced {
		return nil
	}
	if err := s.originals.Remove(cleanupContext, storageKey); err != nil {
		return fmt.Errorf("remove failed registration object: %w", err)
	}
	return nil
}

func suggestedName(fileName string) string {
	if strings.EqualFold(filepath.Ext(fileName), ".pdf") {
		return strings.TrimSuffix(fileName, filepath.Ext(fileName))
	}
	return fileName
}
