// Package search implements grounded semantic search over active document versions.
package search

import (
	"context"
	"errors"
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

type inactiveMatchRepository interface {
	HasInactiveMatch(context.Context, knowledge.Profile, []float32) (bool, error)
}

type embedder interface {
	EmbedQuery(context.Context, string, knowledge.Profile) ([]float32, error)
	Ready(context.Context) error
}

type sourceVerifier interface {
	Verify(context.Context, string, string) error
}

// Service validates, embeds, and executes grounded document searches.
type Service struct {
	repository    repository
	embedder      embedder
	profile       knowledge.Profile
	publicBaseURL string
	exactMatch    bool
	sources       sourceVerifier
}

// GroundingStatus is the deterministic retrieval-safety decision returned to clients.
type GroundingStatus string

const (
	// GroundingSupported means at least one active, above-policy evidence hit is available.
	GroundingSupported GroundingStatus = "SUPPORTED"
	// GroundingInsufficientEvidence means callers must not treat the empty result as grounded evidence.
	GroundingInsufficientEvidence GroundingStatus = "INSUFFICIENT_EVIDENCE"
)

// GroundingReason identifies why the retrieval substrate returned no evidence.
type GroundingReason string

const (
	GroundingNoHitsAbovePolicy          GroundingReason = "NO_HITS_ABOVE_POLICY"
	GroundingOnlyInactiveVersionMatched GroundingReason = "ONLY_INACTIVE_VERSION_MATCHED"
	GroundingSourceUnavailable          GroundingReason = "SOURCE_UNAVAILABLE"
)

// GroundedResult keeps the legacy hits intact while adding an explicit safety decision.
type GroundedResult struct {
	Status GroundingStatus
	Reason GroundingReason
	Hits   []knowledge.SearchHit
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
	sourceVerifiers ...sourceVerifier,
) (*Service, error) {
	parsedBaseURL, err := url.Parse(strings.TrimSpace(publicBaseURL))
	if repository == nil || embedder == nil || err != nil || parsedBaseURL.Host == "" ||
		(parsedBaseURL.Scheme != "http" && parsedBaseURL.Scheme != "https") || parsedBaseURL.User != nil ||
		parsedBaseURL.RawQuery != "" || parsedBaseURL.Fragment != "" ||
		profile.VectorDimension != knowledge.VectorDimension || len(sourceVerifiers) > 1 ||
		(len(sourceVerifiers) == 1 && sourceVerifiers[0] == nil) {
		return nil, fmt.Errorf("configure search: %w", knowledge.ErrInvalidArgument)
	}
	var sources sourceVerifier
	if len(sourceVerifiers) == 1 {
		sources = sourceVerifiers[0]
	}
	return &Service{
		repository:    repository,
		embedder:      embedder,
		profile:       profile,
		publicBaseURL: strings.TrimRight(parsedBaseURL.String(), "/"),
		exactMatch:    exactMatch,
		sources:       sources,
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
	hits, _, err := s.searchActive(ctx, query, limit)
	return hits, err
}

// GroundedDocuments returns only active, above-policy evidence and classifies every
// safe empty result. Retryable dependency failures become SOURCE_UNAVAILABLE with
// an explicit empty slice; invalid input and non-retryable configuration failures
// remain errors.
func (s *Service) GroundedDocuments(
	ctx context.Context,
	query string,
	limit int,
) (GroundedResult, error) {
	hits, vector, err := s.searchActive(ctx, query, limit)
	if err != nil {
		if errors.Is(err, knowledge.ErrTemporarilyUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			return insufficient(GroundingSourceUnavailable), nil
		}
		return GroundedResult{}, err
	}
	if len(hits) > 0 {
		if s.sources == nil {
			return insufficient(GroundingSourceUnavailable), nil
		}
		verified := make(map[struct{ key, digest string }]struct{}, len(hits))
		for _, hit := range hits {
			identity := struct{ key, digest string }{hit.StorageKey, hit.ContentSHA256}
			if _, ok := verified[identity]; ok {
				continue
			}
			if err := s.sources.Verify(ctx, hit.StorageKey, hit.ContentSHA256); err != nil {
				return insufficient(GroundingSourceUnavailable), nil
			}
			verified[identity] = struct{}{}
		}
		return GroundedResult{Status: GroundingSupported, Hits: hits}, nil
	}
	if detector, ok := s.repository.(inactiveMatchRepository); ok {
		matched, probeErr := detector.HasInactiveMatch(ctx, s.profile, vector)
		if probeErr != nil {
			if errors.Is(probeErr, knowledge.ErrTemporarilyUnavailable) || errors.Is(probeErr, context.DeadlineExceeded) {
				return insufficient(GroundingSourceUnavailable), nil
			}
			return GroundedResult{}, fmt.Errorf("check inactive search evidence: %w", probeErr)
		}
		if matched {
			return insufficient(GroundingOnlyInactiveVersionMatched), nil
		}
	}
	return insufficient(GroundingNoHitsAbovePolicy), nil
}

func insufficient(reason GroundingReason) GroundedResult {
	return GroundedResult{
		Status: GroundingInsufficientEvidence,
		Reason: reason,
		Hits:   make([]knowledge.SearchHit, 0),
	}
}

func (s *Service) searchActive(
	ctx context.Context,
	query string,
	limit int,
) ([]knowledge.SearchHit, []float32, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > maxQueryRune {
		return nil, nil, knowledge.ErrInvalidArgument
	}
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return nil, nil, knowledge.ErrInvalidArgument
	}
	vector, err := s.embedder.EmbedQuery(ctx, query, s.profile)
	if err != nil {
		return nil, nil, fmt.Errorf("embed search query: %w", err)
	}
	if len(vector) != knowledge.VectorDimension {
		return nil, nil, knowledge.ErrProfileMismatch
	}
	hits, err := s.repository.Search(ctx, s.profile, vector, limit, s.publicBaseURL)
	if err != nil {
		return nil, vector, fmt.Errorf("search active documents: %w", err)
	}
	if s.exactMatch {
		hits = refineHits(hits, query)
	}
	return hits, vector, nil
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
