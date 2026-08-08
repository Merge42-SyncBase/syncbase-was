package main

import (
	"path/filepath"
	"testing"
)

func TestExtractPagesPreservesKoreanPageHash(t *testing.T) {
	t.Parallel()

	pages, err := extractPages(filepath.Join("testdata", "ko-policy.pdf"))
	if err != nil {
		t.Fatalf("extract pages: %v", err)
	}
	if got, want := len(pages), 1; got != want {
		t.Fatalf("page count=%d, want %d", got, want)
	}
	const wantSHA256 = "716924b85dd0fa8b4f9925ed0211ce2dbb16119fc532d77cbab3b8205d7eb639"
	if got := hashBytes([]byte(pages[0])); got != wantSHA256 {
		t.Errorf("page hash=%s, want %s\ntext: %q", got, wantSHA256, pages[0])
	}
}

func TestExtractPagesRejectsImageOnlyPDF(t *testing.T) {
	t.Parallel()

	path := filepath.Join("testdata", "invalid-image-only.pdf")
	if _, err := extractPages(path); err == nil {
		t.Fatal("extractPages(image-only PDF) succeeded, want error")
	}
}
