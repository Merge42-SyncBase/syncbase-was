// Package embedding adapts the standalone vector engine to WAS domain contracts.
package embedding

import (
	"context"
	"errors"
	"fmt"

	vector "github.com/Merge42-SyncBase/syncbase-embedding"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

// Config identifies the pinned vector model, tokenizer, and runtime artifacts.
type Config = vector.Config

// E5 translates WAS processing profiles and errors at the embedding seam.
type E5 struct {
	engine *vector.E5
}

// New opens the standalone vector engine behind the WAS embedding seam.
func New(config Config) (*E5, error) {
	engine, err := vector.New(config)
	if err != nil {
		return nil, translate(err)
	}
	return &E5{engine: engine}, nil
}

// EmbedQuery creates one normalized query vector for profile.
func (e *E5) EmbedQuery(ctx context.Context, query string, profile knowledge.Profile) ([]float32, error) {
	result, err := e.engine.EmbedQuery(ctx, query, vectorProfile(profile))
	return result, translate(err)
}

// EmbedPassages creates one normalized vector per passage for profile.
func (e *E5) EmbedPassages(
	ctx context.Context,
	passages []string,
	profile knowledge.Profile,
) ([][]float32, error) {
	result, err := e.engine.EmbedPassages(ctx, passages, vectorProfile(profile))
	return result, translate(err)
}

// CountTokens returns the encoded passage length used by the vector engine.
func (e *E5) CountTokens(text string) (int, error) {
	result, err := e.engine.CountTokens(text)
	return result, translate(err)
}

// Ready reports whether the vector engine can accept work.
func (e *E5) Ready(ctx context.Context) error {
	return translate(e.engine.Ready(ctx))
}

// Close releases the local inference runtime.
func (e *E5) Close() error {
	return e.engine.Close()
}

func vectorProfile(profile knowledge.Profile) vector.Profile {
	return vector.Profile{
		EmbeddingModelID: profile.EmbeddingModelID,
		VectorDimension:  profile.VectorDimension,
		Distance:         profile.Distance,
	}
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, vector.ErrInvalidArgument):
		return fmt.Errorf("%v: %w", err, knowledge.ErrInvalidArgument)
	case errors.Is(err, vector.ErrProfileMismatch):
		return fmt.Errorf("%v: %w", err, knowledge.ErrProfileMismatch)
	case errors.Is(err, vector.ErrTemporarilyUnavailable):
		return fmt.Errorf("%v: %w", err, knowledge.ErrTemporarilyUnavailable)
	default:
		return err
	}
}
