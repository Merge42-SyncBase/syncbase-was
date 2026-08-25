// Package objectstore provides the local content-addressed artifact store.
package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Store is a path-confined, content-addressed local artifact store.
type Store struct {
	root string
}

// New returns an artifact store rooted at an existing or newly created directory.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("object root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve object root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("create object root: %w", err)
	}
	return &Store{root: absolute}, nil
}

// Readable verifies that the root directory can be inspected without mutating it.
func (s *Store) Readable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := os.Open(s.root)
	if err != nil {
		return fmt.Errorf("open object root: %w", err)
	}
	if _, err := directory.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		_ = directory.Close()
		return fmt.Errorf("read object root: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close object root: %w", err)
	}
	return ctx.Err()
}

// Ready verifies that the root directory is readable and writable now.
func (s *Store) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("inspect object root: %w", err)
	}
	if !info.IsDir() {
		return errors.New("object root is not a directory")
	}
	temporary, err := os.CreateTemp(s.root, ".syncbase-ready-*")
	if err != nil {
		return fmt.Errorf("create object readiness probe: %w", err)
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temporary.Write([]byte{1}); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write object readiness probe: %w", err)
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("seek object readiness probe: %w", err)
	}
	var content [1]byte
	if _, err := io.ReadFull(temporary, content[:]); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("read object readiness probe: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close object readiness probe: %w", err)
	}
	return ctx.Err()
}

// Put durably stores content once and returns its deterministic relative key.
func (s *Store) Put(ctx context.Context, content []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(content) == 0 {
		return "", errors.New("object content is empty")
	}
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	key := filepath.Join(hash[:2], hash[2:4], hash)
	target, err := s.Path(key)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(target); err == nil {
		return filepath.ToSlash(key), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect original object: %w", err)
	}
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return "", fmt.Errorf("create object directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".syncbase-original-*")
	if err != nil {
		return "", fmt.Errorf("create original temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("set original permissions: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write original: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync original: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close original: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		if _, statErr := os.Stat(target); statErr == nil {
			return filepath.ToSlash(key), nil
		}
		return "", fmt.Errorf("commit original: %w", err)
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return "", fmt.Errorf("open object directory: %w", err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return "", fmt.Errorf("sync object directory: %w", err)
	}
	return filepath.ToSlash(key), nil
}

// Read returns one artifact while preserving cancellation and path confinement.
func (s *Store) Read(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.Path(key)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return content, nil
}

// Verify proves that one confined object is readable and still matches the
// immutable SHA-256 digest recorded at registration time.
func (s *Store) Verify(ctx context.Context, key, expectedSHA256 string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	expected, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(expected) != sha256.Size || strings.ToLower(expectedSHA256) != expectedSHA256 {
		return errors.New("invalid expected object SHA-256")
	}
	path, err := s.Path(key)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open object for verification: %w", err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return fmt.Errorf("read object for verification: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expectedSHA256) {
		return errors.New("object SHA-256 mismatch")
	}
	return nil
}

// Remove deletes one exact artifact. Callers must first prove that no durable
// database record references the content-addressed key.
func (s *Store) Remove(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.Path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove object: %w", err)
	}
	return nil
}

// Path resolves one relative key while preventing traversal outside the root.
func (s *Store) Path(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if key == "" || filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid object key")
	}
	target := filepath.Join(s.root, clean)
	relative, err := filepath.Rel(s.root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid object key")
	}
	return target, nil
}
