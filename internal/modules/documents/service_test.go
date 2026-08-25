package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/objectstore"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
)

func TestRegisterPersistsValidatedOriginal(t *testing.T) {
	t.Parallel()

	content := []byte("%PDF-validated-content")
	repository := &fixtureRepository{registration: knowledge.Registration{DocumentID: uuid.New(), Version: 1}}
	originals := &fixtureOriginalStore{key: "aa/bb/original"}
	parser := &fixtureParser{pages: []knowledge.PageText{{PageNumber: 1, Text: "정책 본문"}}}
	service := newFixtureService(t, repository, originals, parser)

	registration, err := service.Register(context.Background(), RegisterCommand{
		RequestKey:       "register-v1",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     "  정보보안 정책  ",
		OriginalFileName: "policy.pdf",
		Content:          content,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if registration.DocumentID != repository.registration.DocumentID || parser.calls != 1 {
		t.Fatalf("registration=%+v parser calls=%d", registration, parser.calls)
	}
	digest := sha256.Sum256(content)
	if got, want := repository.command.ContentSHA256, hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("ContentSHA256=%q, want %q", got, want)
	}
	if repository.command.DocumentName.Display != "정보보안 정책" ||
		repository.command.StorageKey != originals.key ||
		repository.command.OriginalFileName != "policy.pdf" {
		t.Fatalf("repository command=%+v", repository.command)
	}
}

func TestRegisterGateHonorsTheRequestDeadline(t *testing.T) {
	repository := &fixtureRepository{}
	service := newFixtureService(t, repository, &fixtureOriginalStore{}, &fixtureParser{
		pages: []knowledge.PageText{{PageNumber: 1, Text: "정책 본문"}},
	})
	service.registerGate <- struct{}{}
	defer func() { <-service.registerGate }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := service.Register(ctx, RegisterCommand{
		RequestKey:       "registration-gate-timeout",
		Operation:        knowledge.RegisterNewDocument,
		DocumentName:     "정책",
		OriginalFileName: "policy.pdf",
		Content:          []byte("%PDF-registration-gate-timeout"),
	})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Register error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("Register waited %s for the gate, want a bounded deadline", elapsed)
	}
	if repository.command.RequestKey != "" {
		t.Fatalf("repository Register called after gate timeout: %+v", repository.command)
	}
}

func TestRegisterCreatesRecoverablePendingReservationBeforeParsing(t *testing.T) {
	t.Parallel()

	repository := &fixtureRepository{}
	parser := &fixtureParser{
		pages: []knowledge.PageText{{PageNumber: 1, Text: "정책 본문"}},
		beforeParse: func() {
			if repository.reserveCalls != 1 {
				t.Fatalf("reserve calls before Parse = %d, want 1", repository.reserveCalls)
			}
			recovery, err := repository.RecoverRegistration(context.Background(), "pending-before-parse")
			if err != nil || recovery.State != knowledge.UploadPending {
				t.Fatalf("recovery during Parse = %+v, error=%v, want pending", recovery, err)
			}
		},
	}
	service := newFixtureService(t, repository, &fixtureOriginalStore{key: "aa/bb/original"}, parser)

	if _, err := service.Register(context.Background(), RegisterCommand{
		RequestKey: "pending-before-parse", Operation: knowledge.RegisterNewDocument,
		DocumentName: "정보보안 정책", OriginalFileName: "policy.pdf",
		Content: []byte("%PDF-validated-content"),
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestRegisterExpiresPendingReservationAfterDefinitiveParseRejection(t *testing.T) {
	t.Parallel()

	repository := &fixtureRepository{}
	service := newFixtureService(t, repository, &fixtureOriginalStore{}, &fixtureParser{
		err: knowledge.ErrInvalidPDF,
	})

	_, err := service.Register(context.Background(), RegisterCommand{
		RequestKey: "invalid-after-reservation", Operation: knowledge.RegisterNewDocument,
		DocumentName: "잘못된 PDF", OriginalFileName: "invalid.pdf",
		Content: []byte("%PDF-invalid-content"),
	})
	if !errors.Is(err, knowledge.ErrInvalidPDF) {
		t.Fatalf("Register error=%v, want ErrInvalidPDF", err)
	}
	if repository.expireCalls != 1 {
		t.Fatalf("ExpireRegistration calls=%d, want 1", repository.expireCalls)
	}
}

func TestFindNameMatchesNormalizesForNonBlockingDuplicateGuidance(t *testing.T) {
	t.Parallel()

	documentID := uuid.New()
	repository := &fixtureRepository{
		nameMatchDocuments: []knowledge.DocumentSummary{{
			ID: documentID, Name: "보안 정책", LatestVersion: 2,
			LatestStatus: knowledge.VersionActive,
		}},
		nameMatchTotal: 3,
	}
	service := newFixtureService(t, repository, &fixtureOriginalStore{}, &fixtureParser{})

	matches, err := service.FindNameMatches(context.Background(), "  보안   정책  ", 2)
	if err != nil {
		t.Fatalf("FindNameMatches: %v", err)
	}
	if repository.matchedNormalizedName != "보안 정책" || repository.nameMatchLimit != 2 {
		t.Fatalf("repository lookup name=%q limit=%d", repository.matchedNormalizedName, repository.nameMatchLimit)
	}
	if matches.NormalizedName != "보안 정책" || matches.Total != 3 ||
		len(matches.Documents) != 1 || matches.Documents[0].ID != documentID {
		t.Fatalf("matches=%+v", matches)
	}
}

func TestFindNameMatchesRejectsInvalidNameOrLimit(t *testing.T) {
	t.Parallel()

	service := newFixtureService(t, &fixtureRepository{}, &fixtureOriginalStore{}, &fixtureParser{})
	for _, test := range []struct {
		name  string
		limit int
	}{
		{name: "   ", limit: 3},
		{name: "보안 정책", limit: 0},
		{name: "보안 정책", limit: 11},
	} {
		if _, err := service.FindNameMatches(context.Background(), test.name, test.limit); !errors.Is(err, knowledge.ErrInvalidArgument) {
			t.Errorf("FindNameMatches(%q, %d) error=%v, want ErrInvalidArgument", test.name, test.limit, err)
		}
	}
}

func TestSourceReturnsMetadataOnlyAfterTheOriginalDigestIsVerified(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	content := []byte("%PDF-verified-source")
	digest := sha256.Sum256(content)
	originals, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	storageKey, err := originals.Put(ctx, content)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	documentID := uuid.New()
	repository := &fixtureRepository{source: knowledge.SourceDocument{
		DocumentID: documentID, VersionID: uuid.New(), Version: 1,
		StorageKey: storageKey, ContentSHA256: hex.EncodeToString(digest[:]),
	}}
	service, err := New(repository, originals, &fixtureParser{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := service.Source(ctx, documentID, 1)
	if err != nil {
		t.Fatalf("Source: %v", err)
	}
	wantPath, err := originals.Path(storageKey)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got.Path != wantPath || got.Document.DocumentID != documentID {
		t.Fatalf("source = %+v, want path %q document %s", got, wantPath, documentID)
	}
}

func TestSourceRejectsMissingUnreadableOrCorruptOriginalBeforeReturningMetadata(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			},
		},
		{
			name: "unreadable as a regular source file",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove: %v", err)
				}
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatalf("Mkdir: %v", err)
				}
			},
		},
		{
			name: "SHA-256 mismatch",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("%PDF-corrupt"), 0o640); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			content := []byte("%PDF-source-must-remain-immutable")
			digest := sha256.Sum256(content)
			originals, err := objectstore.New(t.TempDir())
			if err != nil {
				t.Fatalf("objectstore.New: %v", err)
			}
			storageKey, err := originals.Put(ctx, content)
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			path, err := originals.Path(storageKey)
			if err != nil {
				t.Fatalf("Path: %v", err)
			}
			test.mutate(t, path)
			documentID := uuid.New()
			service, err := New(&fixtureRepository{source: knowledge.SourceDocument{
				DocumentID: documentID, VersionID: uuid.New(), Version: 1,
				StorageKey: storageKey, ContentSHA256: hex.EncodeToString(digest[:]),
			}}, originals, &fixtureParser{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			got, err := service.Source(ctx, documentID, 1)
			if !errors.Is(err, knowledge.ErrNotFound) {
				t.Fatalf("Source error = %v, want ErrNotFound", err)
			}
			if got != (Source{}) {
				t.Fatalf("Source = %+v, want no metadata", got)
			}
		})
	}
}

func TestRegisterRemovesUnreferencedOriginalAfterDatabaseFailure(t *testing.T) {
	t.Parallel()

	registerErr := errors.New("database unavailable")
	repository := &fixtureRepository{registerErr: registerErr}
	originals := &fixtureOriginalStore{key: "aa/bb/original"}
	service := newFixtureService(t, repository, originals, &fixtureParser{
		pages: []knowledge.PageText{{PageNumber: 1, Text: "정책 본문"}},
	})

	_, err := service.Register(context.Background(), RegisterCommand{
		RequestKey: "register-v1", Operation: knowledge.RegisterNewDocument,
		DocumentName: "정보보안 정책", OriginalFileName: "policy.pdf",
		Content: []byte("%PDF-validated-content"),
	})
	if !errors.Is(err, registerErr) {
		t.Fatalf("Register error=%v, want database error", err)
	}
	if repository.referenceChecks != 1 || originals.removedKey != originals.key {
		t.Fatalf("reference checks=%d removed=%q", repository.referenceChecks, originals.removedKey)
	}
}

func TestRegisterKeepsOriginalWhenDatabaseReferencesIt(t *testing.T) {
	t.Parallel()

	registerErr := errors.New("commit response lost")
	repository := &fixtureRepository{registerErr: registerErr, referenced: true}
	originals := &fixtureOriginalStore{key: "aa/bb/original"}
	service := newFixtureService(t, repository, originals, &fixtureParser{
		pages: []knowledge.PageText{{PageNumber: 1, Text: "정책 본문"}},
	})

	_, err := service.Register(context.Background(), RegisterCommand{
		RequestKey: "register-v1", Operation: knowledge.RegisterNewDocument,
		DocumentName: "정보보안 정책", OriginalFileName: "policy.pdf",
		Content: []byte("%PDF-validated-content"),
	})
	if !errors.Is(err, registerErr) || originals.removedKey != "" {
		t.Fatalf("error=%v removed=%q", err, originals.removedKey)
	}
}

func TestRegisterDoesNotCleanUpAnObjectThatWasNeverStored(t *testing.T) {
	t.Parallel()

	putErr := errors.New("disk full")
	repository := &fixtureRepository{}
	originals := &fixtureOriginalStore{putErr: putErr}
	service := newFixtureService(t, repository, originals, &fixtureParser{
		pages: []knowledge.PageText{{PageNumber: 1, Text: "정책 본문"}},
	})

	_, err := service.Register(context.Background(), RegisterCommand{
		RequestKey: "register-v1", Operation: knowledge.RegisterNewDocument,
		DocumentName: "정보보안 정책", OriginalFileName: "policy.pdf",
		Content: []byte("%PDF-validated-content"),
	})
	if !errors.Is(err, putErr) {
		t.Fatalf("Register error=%v, want put error", err)
	}
	if repository.referenceChecks != 0 || originals.removedKey != "" {
		t.Fatalf("unexpected cleanup: checks=%d removed=%q", repository.referenceChecks, originals.removedKey)
	}
}

func TestReadyChecksEveryRuntimeDependency(t *testing.T) {
	t.Parallel()

	dependencyErr := errors.New("dependency unavailable")
	tests := []struct {
		name       string
		repository *fixtureRepository
		originals  *fixtureOriginalStore
		parser     *fixtureParser
	}{
		{name: "repository", repository: &fixtureRepository{readyErr: dependencyErr}, originals: &fixtureOriginalStore{}, parser: &fixtureParser{}},
		{name: "original store", repository: &fixtureRepository{}, originals: &fixtureOriginalStore{readyErr: dependencyErr}, parser: &fixtureParser{}},
		{name: "parser", repository: &fixtureRepository{}, originals: &fixtureOriginalStore{}, parser: &fixtureParser{readyErr: dependencyErr}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := newFixtureService(t, test.repository, test.originals, test.parser)
			if err := service.Ready(context.Background()); !errors.Is(err, dependencyErr) {
				t.Fatalf("Ready() error = %v, want dependency error", err)
			}
		})
	}
}

func newFixtureService(
	t *testing.T,
	repository *fixtureRepository,
	originals *fixtureOriginalStore,
	parser *fixtureParser,
) *Service {
	t.Helper()
	service, err := New(repository, originals, parser)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

type fixtureRepository struct {
	command               knowledge.RegisterCommand
	registration          knowledge.Registration
	registerErr           error
	referenced            bool
	referenceErr          error
	referenceChecks       int
	readyErr              error
	nameMatchDocuments    []knowledge.DocumentSummary
	nameMatchTotal        int
	matchedNormalizedName string
	nameMatchLimit        int
	source                knowledge.SourceDocument
	reserveCalls          int
	reservedRequestKey    string
	expireCalls           int
}

func (r *fixtureRepository) ListDocuments(context.Context, int, int) ([]knowledge.DocumentSummary, error) {
	return nil, nil
}

func (r *fixtureRepository) FindDocumentsByNormalizedName(_ context.Context, normalizedName string, limit int) ([]knowledge.DocumentSummary, int, error) {
	r.matchedNormalizedName = normalizedName
	r.nameMatchLimit = limit
	return r.nameMatchDocuments, r.nameMatchTotal, nil
}

func (r *fixtureRepository) GetDocument(context.Context, uuid.UUID) (knowledge.DocumentDetails, error) {
	return knowledge.DocumentDetails{}, knowledge.ErrNotFound
}

func (r *fixtureRepository) GetSource(context.Context, uuid.UUID, int) (knowledge.SourceDocument, error) {
	if r.source.DocumentID == uuid.Nil {
		return knowledge.SourceDocument{}, knowledge.ErrNotFound
	}
	return r.source, nil
}

func (r *fixtureRepository) RecoverRegistration(context.Context, string) (knowledge.UploadRecovery, error) {
	if r.reserveCalls > 0 {
		return knowledge.UploadRecovery{State: knowledge.UploadPending}, nil
	}
	return knowledge.UploadRecovery{State: knowledge.UploadNotCommitted}, nil
}

func (r *fixtureRepository) ReserveRegistration(_ context.Context, command knowledge.ReserveUploadCommand) (knowledge.UploadRecovery, error) {
	r.reserveCalls++
	r.reservedRequestKey = command.RequestKey
	return knowledge.UploadRecovery{State: knowledge.UploadPending}, nil
}

func (r *fixtureRepository) ExpireRegistration(context.Context, knowledge.ReserveUploadCommand) error {
	r.expireCalls++
	return nil
}

func (r *fixtureRepository) StorageKeyReferenced(context.Context, string) (bool, error) {
	r.referenceChecks++
	return r.referenced, r.referenceErr
}

func (r *fixtureRepository) Register(_ context.Context, command knowledge.RegisterCommand) (knowledge.Registration, error) {
	r.command = command
	return r.registration, r.registerErr
}

func (r *fixtureRepository) Retry(context.Context, uuid.UUID, string) (uuid.UUID, error) {
	return uuid.Nil, knowledge.ErrInvalidArgument
}

func (r *fixtureRepository) Ready(context.Context) error {
	return r.readyErr
}

type fixtureOriginalStore struct {
	key        string
	putErr     error
	removeErr  error
	readyErr   error
	removedKey string
	verifyErr  error
}

func (s *fixtureOriginalStore) Put(context.Context, []byte) (string, error) {
	return s.key, s.putErr
}

func (s *fixtureOriginalStore) Path(key string) (string, error) {
	return "/originals/" + key, nil
}

func (s *fixtureOriginalStore) Verify(context.Context, string, string) error {
	return s.verifyErr
}

func (s *fixtureOriginalStore) Remove(_ context.Context, key string) error {
	s.removedKey = key
	return s.removeErr
}

func (s *fixtureOriginalStore) Ready(context.Context) error {
	return s.readyErr
}

type fixtureParser struct {
	pages       []knowledge.PageText
	err         error
	readyErr    error
	calls       int
	beforeParse func()
}

func (p *fixtureParser) Parse(context.Context, []byte) ([]knowledge.PageText, error) {
	if p.beforeParse != nil {
		p.beforeParse()
	}
	p.calls++
	return p.pages, p.err
}

func (p *fixtureParser) Ready(context.Context) error {
	return p.readyErr
}
