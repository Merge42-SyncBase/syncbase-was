package pdf_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/pdf"
)

func TestParserPreservesQualifiedKoreanPage(t *testing.T) {
	t.Parallel()

	fixturePath := filepath.Join("testdata", "ko-policy.pdf")
	if _, err := os.Stat(fixturePath); err != nil {
		fixturePath = filepath.Join(
			"..", "..", "..", "output", "pdf", "syncbase-pdf-corpus-v1", "ko-policy.pdf",
		)
	}

	parser, err := pdf.New(context.Background())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = parser.Close() })
	pages, err := parser.ParseFile(context.Background(), fixturePath)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if got, want := len(pages), 1; got != want {
		t.Fatalf("page count = %d, want %d", got, want)
	}
	const wantSHA256 = "716924b85dd0fa8b4f9925ed0211ce2dbb16119fc532d77cbab3b8205d7eb639"
	if got := pdf.TextSHA256(pages[0].Text); got != wantSHA256 {
		t.Errorf("page text hash = %s, want %s", got, wantSHA256)
	}
}
