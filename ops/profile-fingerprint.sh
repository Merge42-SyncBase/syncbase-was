#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <minimum-score>" >&2
  exit 64
fi

minimum_score="$(LC_ALL=C awk -v value="$1" '
  BEGIN {
    if (value !~ /^([0-9]+)(\.[0-9]+)?$/ || value < 0 || value > 1) exit 1
    printf "%.6f", value
  }
')" || { echo "minimum score must be between 0 and 1" >&2; exit 64; }
model_sha="ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
tokenizer_sha="0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"
onnx_runtime_id="onnxruntime-1.26.0"
canonical="$(printf '%s' \
  "{\"chunker_id\":\"page-aware-recursive-v1\",\"distance\":\"cosine\",\"embedding_model_id\":\"intfloat/multilingual-e5-small\",\"embedding_model_sha256\":\"$model_sha\",\"minimum_score\":$minimum_score,\"onnx_runtime_id\":\"$onnx_runtime_id\",\"parser_id\":\"pdfium-wasm-1.19.6\",\"tokenizer_sha256\":\"$tokenizer_sha\",\"vector_dimension\":384}")"

if command -v sha256sum >/dev/null 2>&1; then
  fingerprint="$(printf '%s' "$canonical" | sha256sum | awk '{print $1}')"
else
  fingerprint="$(printf '%s' "$canonical" | shasum -a 256 | awk '{print $1}')"
fi
printf 'profile_fingerprint=%s\ncanonical_json=%s\n' "$fingerprint" "$canonical"
