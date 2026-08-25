package search_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/objectstore"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/search"
)

func TestDocumentsOwnsValidationEmbeddingAndRepositoryPolicy(t *testing.T) {
	profile := knowledge.Profile{VectorDimension: knowledge.VectorDimension}
	repository := &recordingRepository{}
	embedder := &recordingEmbedder{}
	service, err := search.New(repository, embedder, profile, "https://docs.example.test/", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := service.Documents(context.Background(), "  장애 대응  ", 0); err != nil {
		t.Fatalf("Documents: %v", err)
	}
	if embedder.query != "장애 대응" {
		t.Errorf("embedded query = %q, want %q", embedder.query, "장애 대응")
	}
	if repository.limit != 5 || repository.baseURL != "https://docs.example.test" {
		t.Errorf("repository policy = limit %d, base URL %q", repository.limit, repository.baseURL)
	}
}

func TestDocumentsRejectsInvalidInputBeforeEmbedding(t *testing.T) {
	profile := knowledge.Profile{VectorDimension: knowledge.VectorDimension}
	embedder := &recordingEmbedder{}
	service, err := search.New(&recordingRepository{}, embedder, profile, "https://docs.example.test", false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, test := range []struct {
		name  string
		query string
		limit int
	}{
		{name: "blank query", query: "  ", limit: 5},
		{name: "negative limit", query: "valid", limit: -1},
		{name: "limit above maximum", query: "valid", limit: 21},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.Documents(context.Background(), test.query, test.limit); !errors.Is(err, knowledge.ErrInvalidArgument) {
				t.Fatalf("Documents error = %v, want ErrInvalidArgument", err)
			}
		})
	}
	if embedder.calls != 0 {
		t.Fatalf("EmbedQuery calls = %d, want 0", embedder.calls)
	}
}

func TestGroundedDocumentsClassifiesEverySafetyStateWithoutLeakingEvidence(t *testing.T) {
	profile := knowledge.Profile{VectorDimension: knowledge.VectorDimension}
	hit := knowledge.SearchHit{Rank: 1, Snippet: "검증 가능한 활성 근거"}

	tests := []struct {
		name           string
		repository     *recordingRepository
		wantStatus     search.GroundingStatus
		wantReason     search.GroundingReason
		wantHits       int
		wantErr        error
		wantProbeCalls int
	}{
		{
			name:       "supported active evidence",
			repository: &recordingRepository{hits: []knowledge.SearchHit{hit}},
			wantStatus: search.GroundingSupported, wantHits: 1,
		},
		{
			name:           "no hits above policy",
			repository:     &recordingRepository{},
			wantStatus:     search.GroundingInsufficientEvidence,
			wantReason:     search.GroundingNoHitsAbovePolicy,
			wantProbeCalls: 1,
		},
		{
			name:           "only inactive version matched",
			repository:     &recordingRepository{inactiveMatch: true},
			wantStatus:     search.GroundingInsufficientEvidence,
			wantReason:     search.GroundingOnlyInactiveVersionMatched,
			wantProbeCalls: 1,
		},
		{
			name:       "active source unavailable",
			repository: &recordingRepository{failure: knowledge.ErrTemporarilyUnavailable},
			wantStatus: search.GroundingInsufficientEvidence,
			wantReason: search.GroundingSourceUnavailable,
		},
		{
			name:           "inactive safety probe unavailable",
			repository:     &recordingRepository{inactiveFailure: knowledge.ErrTemporarilyUnavailable},
			wantStatus:     search.GroundingInsufficientEvidence,
			wantReason:     search.GroundingSourceUnavailable,
			wantProbeCalls: 1,
		},
		{
			name:       "non dependency failure remains an error",
			repository: &recordingRepository{failure: knowledge.ErrProfileMismatch},
			wantErr:    knowledge.ErrProfileMismatch,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := search.New(test.repository, &recordingEmbedder{}, profile, "https://docs.example.test", false)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := service.GroundedDocuments(context.Background(), "정책", 5)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("GroundedDocuments error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GroundedDocuments: %v", err)
			}
			if got.Status != test.wantStatus || got.Reason != test.wantReason || len(got.Hits) != test.wantHits {
				t.Fatalf("result = %#v, want status=%q reason=%q hits=%d", got, test.wantStatus, test.wantReason, test.wantHits)
			}
			if test.wantStatus == search.GroundingInsufficientEvidence && got.Hits == nil {
				t.Fatal("insufficient-evidence hits must serialize as an empty array, not null")
			}
			if test.repository.inactiveProbeCalls != test.wantProbeCalls {
				t.Fatalf("inactive probe calls = %d, want %d", test.repository.inactiveProbeCalls, test.wantProbeCalls)
			}
		})
	}
}

func TestGroundedDocumentsSupportsEvidenceWhoseOriginalMatchesItsDigest(t *testing.T) {
	t.Parallel()

	content := []byte("%PDF-trusted-original")
	digest := sha256.Sum256(content)
	originals, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("objectstore.New: %v", err)
	}
	storageKey, err := originals.Put(context.Background(), content)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	repository := &recordingRepository{hits: []knowledge.SearchHit{{
		Rank: 1, Snippet: "검증 가능한 활성 근거",
		StorageKey: storageKey, ContentSHA256: hex.EncodeToString(digest[:]),
	}}}
	service, err := search.New(
		repository, &recordingEmbedder{},
		knowledge.Profile{VectorDimension: knowledge.VectorDimension},
		"https://docs.example.test", false, originals,
	)
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}

	got, err := service.GroundedDocuments(context.Background(), "정책", 5)
	if err != nil {
		t.Fatalf("GroundedDocuments: %v", err)
	}
	if got.Status != search.GroundingSupported || len(got.Hits) != 1 {
		t.Fatalf("result = %#v, want one supported hit", got)
	}
}

func TestDocumentsExactMatchPromotesTokenSnippets(t *testing.T) {
	profile := knowledge.Profile{VectorDimension: knowledge.VectorDimension}
	factory := func(exactMatch bool, hits []knowledge.SearchHit) *service {
		service, err := search.New(&recordingRepository{hits: hits}, &recordingEmbedder{}, profile, "https://docs.example.test/", exactMatch)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return service
	}
	mixed := []knowledge.SearchHit{
		{Rank: 1, Snippet: "민영재와 신은영이 함께"}, // contains 신은영
		{Rank: 2, Snippet: "정책과 규정의 관계"},   // noise
		{Rank: 3, Snippet: "신은영은 팀장"},      // contains 신은영
		{Rank: 4, Snippet: "2026년 대회 운영"},  // noise
	}

	t.Run("exact_match_keeps_only_snippets_containing_token", func(t *testing.T) {
		s := factory(true, mixed)
		got, err := s.Documents(context.Background(), "신은영", 10)
		if err != nil {
			t.Fatalf("Documents: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		want := []string{"민영재와 신은영이 함께", "신은영은 팀장"}
		for i := range got {
			if got[i].Snippet != want[i] || got[i].Rank != i+1 {
				t.Fatalf("hit[%d] = snippet%q rank%d, want %q rank%d", i, got[i].Snippet, got[i].Rank, want[i], i+1)
			}
		}
	})

	t.Run("exact_match_falls_back_to_semantic_when_no_snippet_matches", func(t *testing.T) {
		// Single-token query (3+ chars, no space) that appears in NO snippet.
		// refineHits must fall through to the original semantic top-N.
		s := factory(true, mixed)
		got, err := s.Documents(context.Background(), "김철수", 10)
		if err != nil {
			t.Fatalf("Documents: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("len = %d, want 4 (fuzzy fallback)", len(got))
		}
	})

	t.Run("short_single_char_query_keeps_semantic", func(t *testing.T) {
		// A single-token query of length 1 should not trigger refinement.
		s := factory(true, mixed)
		got, err := s.Documents(context.Background(), "a", 10)
		if err != nil {
			t.Fatalf("Documents: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("len = %d, want 4 (short query keeps semantic)", len(got))
		}
	})

	t.Run("multi_token_query_is_left_unchanged", func(t *testing.T) {
		// "정책 개정" — paraphrase, multi-token. refineHits must NOT filter
		// regardless of whether "정책" appears in any snippet.
		s := factory(true, mixed)
		got, err := s.Documents(context.Background(), "정책 개정", 10)
		if err != nil {
			t.Fatalf("Documents: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("len = %d, want 4 (multi-token query keeps all semantic)", len(got))
		}
	})

	t.Run("exact_match_off_keeps_all", func(t *testing.T) {
		s := factory(false, mixed)
		got, err := s.Documents(context.Background(), "신은영", 10)
		if err != nil {
			t.Fatalf("Documents: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("len = %d, want 4 (refinement off)", len(got))
		}
	})
}

type service = search.Service

func TestNewRejectsUnsafePublicBaseURLs(t *testing.T) {
	t.Parallel()

	profile := knowledge.Profile{VectorDimension: knowledge.VectorDimension}
	for _, baseURL := range []string{
		"", "localhost:8080", "file:///tmp/source", "https://user@example.test", "https://example.test?tenant=one",
	} {
		t.Run(baseURL, func(t *testing.T) {
			t.Parallel()
			if _, err := search.New(&recordingRepository{}, &recordingEmbedder{}, profile, baseURL, false); !errors.Is(err, knowledge.ErrInvalidArgument) {
				t.Fatalf("New(%q) error = %v, want ErrInvalidArgument", baseURL, err)
			}
		})
	}
}

type recordingEmbedder struct {
	query string
	calls int
}

func (e *recordingEmbedder) EmbedQuery(_ context.Context, query string, _ knowledge.Profile) ([]float32, error) {
	e.calls++
	e.query = query
	return make([]float32, knowledge.VectorDimension), nil
}

func (e *recordingEmbedder) Ready(context.Context) error {
	return nil
}

type recordingRepository struct {
	limit              int
	baseURL            string
	hits               []knowledge.SearchHit
	failure            error
	inactiveMatch      bool
	inactiveFailure    error
	inactiveProbeCalls int
}

func (r *recordingRepository) Search(
	_ context.Context,
	_ knowledge.Profile,
	_ []float32,
	limit int,
	baseURL string,
) ([]knowledge.SearchHit, error) {
	r.limit = limit
	r.baseURL = baseURL
	if r.failure != nil {
		return nil, r.failure
	}
	if r.hits == nil {
		return nil, nil
	}
	return r.hits, nil
}

func (r *recordingRepository) HasInactiveMatch(
	_ context.Context,
	_ knowledge.Profile,
	_ []float32,
) (bool, error) {
	r.inactiveProbeCalls++
	return r.inactiveMatch, r.inactiveFailure
}
