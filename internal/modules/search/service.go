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
	exactMatch    bool
}

// New returns a search service backed by the provided repository and embedder.
// exactMatch=true enables query-time exact-token refinement: when at least one
// returned hit's snippet contains the full query as a substring, only those
// hits are kept; otherwise the full semantic top-N is returned unchanged.
func New(
	repository repository,
	embedder embedder,
	profile knowledge.Profile,
	publicBaseURL string,
	exactMatch bool,
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
		exactMatch:    exactMatch,
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
	if !s.exactMatch {
		return hits, nil
	}
	return refineHits(hits, query), nil
}

// refineHits promotes the top semantic candidates that actually contain the
// full query string as a substring of their snippet.
//
// Rationale: a proper RAG should surface, when present, any chunk whose text
// contains the user's exact query token (a personal name, a code, an ID).
// E5-small is a semantic model: it will happily return near-synonym chunks at
// 0.87–0.89 similarity alongside the genuine token hit. Filtering those out at
// search time — *not* by raising the profile's minimum_score (which is locked
// into the fingerprint that keys chunks and run profiles) — is the correct lever.
//
// Scope is intentionally narrow to avoid side effects on multi-word or
// paraphrase queries:
//   - single-token queries  →  exact substring containment of the snippet.
//     This is the "신은영" case: the name is a token that appears verbatim in
//     the chunk, and any chunk without that token is noise for this query.
//   - multi-token queries   →  keep the original semantic top-N unchanged.
//     Splitting by word and requiring "at least one token to appear" would
//     over-suppress legitimate near-synonym chunks ("정책 개정" → chunks with
//     "규정 수정" should not be dropped). Multi-word search is a paraphrase
//     and E5's similarity is the right signal there.
//
// When NO snippet in the top-N contains the token (single-token query), the
// original top-N is returned unchanged — we never collapse a semantic recall
// to an empty result.
func refineHits(hits []knowledge.SearchHit, query string) []knowledge.SearchHit {
	tokens := strings.Fields(query)
	if len(tokens) != 1 || len(tokens[0]) < 2 {
		return hits // multi-token or too-short → keep semantic top-N
	}
	token := tokens[0]
	exact := make([]knowledge.SearchHit, 0, len(hits))
	for _, hit := range hits {
		if strings.Contains(hit.Snippet, token) {
			exact = append(exact, hit)
		}
	}
	if len(exact) == 0 {
		return hits
	}
	for index := range exact {
		exact[index].Rank = index + 1
	}
	return exact
}
