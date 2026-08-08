package knowledge_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileFingerprintScriptMatchesGoRuntime(t *testing.T) {
	t.Parallel()

	script := filepath.Join("..", "..", "..", "ops", "profile-fingerprint.sh")
	output, err := exec.Command("bash", script, "0.62").CombinedOutput()
	if err != nil {
		t.Fatalf("profile script failed: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, "profile_fingerprint=7e8430367ed8b10188d3806d07bd4a344228ffc80e3597d52cafbc936f6a8f61") {
		t.Fatalf("profile script fingerprint does not match Go runtime:\n%s", text)
	}
	if !strings.Contains(text, `"parser_id":"pdfium-wasm-1.19.6"`) {
		t.Fatalf("profile script parser does not match Go runtime:\n%s", text)
	}
	if !strings.Contains(text, `"onnx_runtime_id":"onnxruntime-1.26.0"`) {
		t.Fatalf("profile script runtime does not match Go runtime:\n%s", text)
	}
}
