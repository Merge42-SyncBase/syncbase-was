package documents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

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
	return knowledge.SourceDocument{}, knowledge.ErrNotFound
}

func (r *fixtureRepository) RecoverRegistration(context.Context, string) (knowledge.UploadRecovery, error) {
	return knowledge.UploadRecovery{State: knowledge.UploadNotCommitted}, nil
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
}

func (s *fixtureOriginalStore) Put(context.Context, []byte) (string, error) {
	return s.key, s.putErr
}

func (s *fixtureOriginalStore) Path(key string) (string, error) {
	return "/originals/" + key, nil
}

func (s *fixtureOriginalStore) Remove(_ context.Context, key string) error {
	s.removedKey = key
	return s.removeErr
}

func (s *fixtureOriginalStore) Ready(context.Context) error {
	return s.readyErr
}

type fixtureParser struct {
	pages    []knowledge.PageText
	err      error
	readyErr error
	calls    int
}

func (p *fixtureParser) Parse(context.Context, []byte) ([]knowledge.PageText, error) {
	p.calls++
	return p.pages, p.err
}

func (p *fixtureParser) Ready(context.Context) error {
	return p.readyErr
}
