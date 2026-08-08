package searchruntime

import (
	"context"
	"errors"
	"testing"
)

func TestOpenRejectsIncompleteConfigBeforeIO(t *testing.T) {
	t.Parallel()

	_, err := Open(context.Background(), Config{})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Open error = %v, want ErrInvalidArgument", err)
	}
}
