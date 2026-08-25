package knowledge_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

func TestProfileFingerprintScriptMatchesGoRuntime(t *testing.T) {
	t.Parallel()

	script := filepath.Join("..", "..", "..", "ops", "profile-fingerprint.sh")
	const modelSHA = "ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
	const tokenizerSHA = "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39" // gitleaks:allow -- public deterministic tokenizer SHA-256 fixture, not a credential

	for _, score := range []string{"0.62", "0.93"} {
		t.Run(score, func(t *testing.T) {
			profile, canonical, err := knowledge.NewProfile(
				modelSHA,
				tokenizerSHA,
				"onnxruntime-1.26.0",
				mustParseScore(t, score),
			)
			if err != nil {
				t.Fatalf("build Go profile: %v", err)
			}

			output, err := exec.Command("bash", script, score).CombinedOutput()
			if err != nil {
				t.Fatalf("profile script failed: %v\n%s", err, output)
			}
			text := string(output)
			if !strings.Contains(text, "profile_fingerprint="+profile.Fingerprint+"\n") {
				t.Fatalf("profile script fingerprint does not match Go runtime:\n%s", text)
			}
			if !strings.Contains(text, "canonical_json="+canonical+"\n") {
				t.Fatalf("profile script canonical JSON does not match Go runtime:\n%s", text)
			}
		})
	}
}

func mustParseScore(t *testing.T, value string) float64 {
	t.Helper()
	var score float64
	if _, err := fmt.Sscan(value, &score); err != nil {
		t.Fatalf("parse test score %q: %v", value, err)
	}
	return score
}
