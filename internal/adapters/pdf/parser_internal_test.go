package pdf

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

func TestReadPDFContentRejectsOversizedInput(t *testing.T) {
	_, err := readPDFContent(bytes.NewReader([]byte("1234")), 3)
	if !errors.Is(err, knowledge.ErrInvalidPDF) {
		t.Fatalf("readPDFContent() error = %v, want invalid PDF", err)
	}
}
