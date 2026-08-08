package processing_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/processing"
)

func TestChunkPagesNeverCrossesPageBoundary(t *testing.T) {
	t.Parallel()

	pages := []knowledge.PageText{
		{PageNumber: 1, Text: strings.Repeat("첫 페이지 문장입니다. ", 200)},
		{PageNumber: 2, Text: strings.Repeat("둘째 페이지 문장입니다. ", 200)},
	}
	chunks := processing.ChunkPages(pages)
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want at least 2", len(chunks))
	}
	for index, chunk := range chunks {
		if chunk.Index != index {
			t.Errorf("chunk index = %d, want %d", chunk.Index, index)
		}
		if chunk.PageNumber != 1 && chunk.PageNumber != 2 {
			t.Errorf("page number = %d, want 1 or 2", chunk.PageNumber)
		}
	}
}

func TestChunkPagesSkipsBlankPages(t *testing.T) {
	t.Parallel()

	chunks := processing.ChunkPages([]knowledge.PageText{
		{PageNumber: 1, Text: "  "},
		{PageNumber: 2, Text: "실제 근거입니다."},
	})
	if got, want := len(chunks), 1; got != want {
		t.Fatalf("chunk count = %d, want %d", got, want)
	}
	if got, want := chunks[0].PageNumber, 2; got != want {
		t.Fatalf("page number = %d, want %d", got, want)
	}
}

func TestChunkPagesWithCounterUsesTokenizerLimitAndOverlap(t *testing.T) {
	words := make([]string, 1200)
	for index := range words {
		words[index] = fmt.Sprintf("token-%04d", index)
	}
	counter := func(text string) (int, error) { return len(strings.Fields(text)), nil }
	chunks, err := processing.ChunkPagesWithCounter([]knowledge.PageText{{
		PageNumber: 1, Text: strings.Join(words, " "),
	}}, counter)
	if err != nil {
		t.Fatalf("ChunkPagesWithCounter: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("chunk count = %d, want at least 3", len(chunks))
	}
	for _, chunk := range chunks {
		count, _ := counter(chunk.Text)
		if count > 480 {
			t.Fatalf("chunk %d tokens = %d, want <= 480", chunk.Index, count)
		}
	}
	left := strings.Fields(chunks[0].Text)
	right := strings.Fields(chunks[1].Text)
	if len(left) < 64 || len(right) < 64 ||
		strings.Join(left[len(left)-64:], " ") != strings.Join(right[:64], " ") {
		t.Fatalf("forced split does not preserve 64-token overlap")
	}
}
