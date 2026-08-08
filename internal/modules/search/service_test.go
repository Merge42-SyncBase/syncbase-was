package search_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/search"
)

func TestDocumentsOwnsValidationEmbeddingAndRepositoryPolicy(t *testing.T) {
	profile := knowledge.Profile{VectorDimension: knowledge.VectorDimension}
	repository := &recordingRepository{}
	embedder := &recordingEmbedder{}
	service, err := search.New(repository, embedder, profile, "https://docs.example.test/")
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
	service, err := search.New(&recordingRepository{}, embedder, profile, "https://docs.example.test")
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

func TestNewRejectsUnsafePublicBaseURLs(t *testing.T) {
	t.Parallel()

	profile := knowledge.Profile{VectorDimension: knowledge.VectorDimension}
	for _, baseURL := range []string{
		"", "localhost:8080", "file:///tmp/source", "https://user@example.test", "https://example.test?tenant=one",
	} {
		t.Run(baseURL, func(t *testing.T) {
			t.Parallel()
			if _, err := search.New(&recordingRepository{}, &recordingEmbedder{}, profile, baseURL); !errors.Is(err, knowledge.ErrInvalidArgument) {
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
	limit   int
	baseURL string
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
	return nil, nil
}
