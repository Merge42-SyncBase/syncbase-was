package knowledge_test

import (
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

func TestNewDocumentNameNormalizesForDuplicateGuidance(t *testing.T) {
	t.Parallel()

	name, err := knowledge.NewDocumentName("  보안   정책  ")
	if err != nil {
		t.Fatalf("NewDocumentName: %v", err)
	}
	if got, want := name.Display, "보안   정책"; got != want {
		t.Errorf("Display = %q, want %q", got, want)
	}
	if got, want := name.Normalized, "보안 정책"; got != want {
		t.Errorf("Normalized = %q, want %q", got, want)
	}
}

func TestNewDocumentNameRejectsEmptyAndOversizedValues(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"   ", string(make([]rune, 201))} {
		if _, err := knowledge.NewDocumentName(value); err == nil {
			t.Errorf("NewDocumentName(%q) succeeded, want error", value)
		}
	}
}

func TestScoreFromCosineDistanceUsesStableContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		distance float64
		want     float64
	}{
		{name: "identical", distance: 0, want: 1},
		{name: "orthogonal", distance: 1, want: 0.5},
		{name: "opposite", distance: 2, want: 0},
		{name: "clamps negative", distance: -1, want: 1},
		{name: "clamps large", distance: 3, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := knowledge.ScoreFromCosineDistance(test.distance); got != test.want {
				t.Fatalf("ScoreFromCosineDistance(%v) = %v, want %v", test.distance, got, test.want)
			}
		})
	}
}
