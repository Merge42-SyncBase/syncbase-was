package config

import (
	"encoding/hex"
	"testing"
)

func TestRuntimeLibrarySHA256IsPinnedForTestPlatform(t *testing.T) {
	t.Parallel()

	digest, err := RuntimeLibrarySHA256()
	if err != nil {
		t.Fatalf("RuntimeLibrarySHA256: %v", err)
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("runtime digest = %q, want SHA-256", digest)
	}
}
