package knowledge_test

import (
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

func TestNewProfileProducesStableFingerprint(t *testing.T) {
	t.Parallel()

	profile, canonical, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	const wantCanonical = `{"chunk_overlap_tokens":64,"chunk_size_tokens":384,"chunker_id":"page-aware-recursive-v1","distance":"cosine","embedding_model_id":"intfloat/multilingual-e5-small","embedding_model_sha256":"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665","minimum_score":0.620000,"onnx_runtime_id":"onnxruntime-1.26.0","parser_id":"pdfium-wasm-1.19.6","provider":"local-onnx","tokenizer_sha256":"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39","vector_dimension":384}`
	if canonical != wantCanonical {
		t.Fatalf("canonical profile = %s, want %s", canonical, wantCanonical)
	}
	const wantFingerprint = "3fa706a188c6118549f2187b78b078c01feac7b0877dbac5d5f2486173715352"
	if profile.Fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %s, want %s", profile.Fingerprint, wantFingerprint)
	}
	if profile.Provider != "local-onnx" || profile.ChunkSizeTokens != 384 || profile.ChunkOverlapTokens != 64 {
		t.Fatalf("embedding profile metadata = %+v", profile)
	}
}
