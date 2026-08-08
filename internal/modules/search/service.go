// Package search implements grounded semantic search over active document versions.
package search

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

const (
	defaultLimit = 5
	maxLimit     = 20
	maxQueryRune = 2000
)

type repository interface {
	Search(context.Context, knowledge.Profile, []float32, int, string) ([]knowledge.SearchHit, error)
}

type embedder interface {
	EmbedQuery(context.Context, string, knowledge.Profile) ([]float32, error)
	Ready(context.Context) error
}

// Service validates, embeds, and executes grounded document searches.
type Service struct {
	repository    repository
	embedder      embedder
	profile       knowledge.Profile
	publicBaseURL string
}

// New returns a search service backed by the provided repository and embedder.
func New(
	repository repository,
	embedder embedder,
	profile knowledge.Profile,
	publicBaseURL string,
) (*Service, error) {
	parsedBaseURL, err := url.Parse(strings.TrimSpace(publicBaseURL))
	if repository == nil || embedder == nil || err != nil || parsedBaseURL.Host == "" ||
		(parsedBaseURL.Scheme != "http" && parsedBaseURL.Scheme != "https") || parsedBaseURL.User != nil ||
		parsedBaseURL.RawQuery != "" || parsedBaseURL.Fragment != "" ||
		profile.VectorDimension != knowledge.VectorDimension {
		return nil, fmt.Errorf("configure search: %w", knowledge.ErrInvalidArgument)
	}
	return &Service{
		repository:    repository,
		embedder:      embedder,
		profile:       profile,
		publicBaseURL: strings.TrimRight(parsedBaseURL.String(), "/"),
	}, nil
}

// Ready reports whether the query embedder can accept work.
func (s *Service) Ready(ctx context.Context) error {
	if err := s.embedder.Ready(ctx); err != nil {
		return fmt.Errorf("search embedder readiness: %w", err)
	}
	return nil
}

// Documents returns ranked hits from active document versions only.
// A zero limit uses the default; other limits must be between 1 and 20.
func (s *Service) Documents(
	ctx context.Context,
	query string,
	limit int,
) ([]knowledge.SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > maxQueryRune {
		return nil, knowledge.ErrInvalidArgument
	}
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return nil, knowledge.ErrInvalidArgument
	}
	vector, err := s.embedder.EmbedQuery(ctx, query, s.profile)
	if err != nil {
		return nil, fmt.Errorf("embed search query: %w", err)
	}
	if len(vector) != knowledge.VectorDimension {
		return nil, knowledge.ErrProfileMismatch
	}
	hits, err := s.repository.Search(ctx, s.profile, vector, limit, s.publicBaseURL)
	if err != nil {
		return nil, fmt.Errorf("search active documents: %w", err)
	}
	return hits, nil
}
