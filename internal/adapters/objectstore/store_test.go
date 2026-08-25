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

func TestReadableAcceptsReadOnlyRootWithoutMutation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := objectstore.New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := os.WriteFile(root+"/existing-original", []byte("source"), 0o440); err != nil {
		t.Fatalf("seed source root: %v", err)
	}
	if err := os.Chmod(root, 0o550); err != nil {
		t.Fatalf("make source root read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o750) })

	if err := store.Readable(context.Background()); err != nil {
		t.Fatalf("Readable with read-only root: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir after Readable: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "existing-original" {
		t.Fatalf("source-root entries after Readable = %v, want only existing-original", entries)
	}
}
