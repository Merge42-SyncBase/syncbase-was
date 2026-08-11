package embedding

import (
	"errors"
	"testing"

	vector "github.com/Merge42-SyncBase/syncbase-embedding"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

func TestTranslatePreservesWASErrorContract(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		input  error
		target error
	}{
		{name: "invalid argument", input: vector.ErrInvalidArgument, target: knowledge.ErrInvalidArgument},
		{name: "profile mismatch", input: vector.ErrProfileMismatch, target: knowledge.ErrProfileMismatch},
		{name: "temporarily unavailable", input: vector.ErrTemporarilyUnavailable, target: knowledge.ErrTemporarilyUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := translate(test.input); !errors.Is(err, test.target) {
				t.Fatalf("translate(%v) = %v, want errors.Is(_, %v)", test.input, err, test.target)
			}
		})
	}
}

func TestVectorProfilePreservesTheCompleteIndexingContract(t *testing.T) {
	t.Parallel()

	profile := knowledge.Profile{
		Provider: "local-onnx", EmbeddingModelID: "intfloat/multilingual-e5-small",
		VectorDimension: knowledge.VectorDimension, Distance: "cosine",
		ChunkSizeTokens: 384, ChunkOverlapTokens: 64,
	}
	got := vectorProfile(profile)
	if got.Provider != profile.Provider || got.ChunkSizeTokens != profile.ChunkSizeTokens ||
		got.ChunkOverlapTokens != profile.ChunkOverlapTokens {
		t.Fatalf("vector profile = %+v, want provider and chunk contract from %+v", got, profile)
	}
}
