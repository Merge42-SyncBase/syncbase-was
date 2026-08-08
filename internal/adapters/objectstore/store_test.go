package objectstore_test

import (
	"context"
	"os"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/adapters/objectstore"
)

func TestPutIsContentAddressedAndIdempotent(t *testing.T) {
	t.Parallel()

	store, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	content := []byte("qualified PDF bytes")
	first, err := store.Put(context.Background(), content)
	if err != nil {
		t.Fatalf("Put first: %v", err)
	}
	second, err := store.Put(context.Background(), content)
	if err != nil {
		t.Fatalf("Put repeated: %v", err)
	}
	if first != second {
		t.Fatalf("storage keys differ: %q != %q", first, second)
	}
	path, err := store.Path(first)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
}

func TestPathRejectsTraversal(t *testing.T) {
	t.Parallel()

	store, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := store.Path("../../secret"); err == nil {
		t.Fatal("Path traversal succeeded, want error")
	}
}

func TestReadyVerifiesRootAccess(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := objectstore.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.Ready(context.Background()); err != nil {
		t.Fatalf("Ready with accessible root: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll root: %v", err)
	}
	if err := store.Ready(context.Background()); err == nil {
		t.Fatal("Ready with missing root = nil, want error")
	}
}
