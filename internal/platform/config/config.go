// Package config centralizes strict runtime configuration for SyncBase binaries.
package config

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

const (
	// ModelSHA256 pins the only accepted multilingual-E5 model artifact.
	ModelSHA256 = "ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
	// TokenizerSHA256 pins the only accepted multilingual-E5 tokenizer artifact.
	TokenizerSHA256 = "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"
	// ONNXRuntimeID identifies the platform-independent inference runtime in the processing profile.
	ONNXRuntimeID = "onnxruntime-1.26.0"
)

// RuntimeLibrarySHA256 returns the pinned ONNX Runtime shared-library digest
// for the current process architecture.
func RuntimeLibrarySHA256() (string, error) {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	switch platform {
	case "darwin/arm64":
		return "cb0462c3fd35ad722e8772313030a33c182f3d4c6b33f4e5e1fcb2ce3199b86c", nil
	case "linux/amd64":
		return "5bd5bedf736fc501692435d0ec4f6e8b2bdf48cd30af8e6d00d61b3ddc9a7ab8", nil
	case "linux/arm64":
		return "115ecb838e703d390262b8b4d07d5248e6693c67658d4c98c48f94905ab27af4", nil
	default:
		return "", fmt.Errorf("unsupported ONNX Runtime platform %s", platform)
	}
}

// Required returns a non-empty environment value or an error.
func Required(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

// Value returns an environment value or its fallback when empty.
func Value(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// Bool parses an environment boolean or returns its fallback when empty.
func Bool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

// Duration parses an environment duration or returns its fallback when empty.
func Duration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("parse %s: positive Go duration required", name)
	}
	return parsed, nil
}

// CSV returns trimmed, non-empty values from a comma-separated environment value.
func CSV(name string, fallback []string) []string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

// Profile builds the immutable processing profile from pinned runtime constants.
func Profile() (knowledge.Profile, string, error) {
	minimum := 0.62
	if value := strings.TrimSpace(os.Getenv("SYNCBASE_MINIMUM_SCORE")); value != "" {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return knowledge.Profile{}, "", fmt.Errorf("parse SYNCBASE_MINIMUM_SCORE: %w", err)
		}
		minimum = parsed
	}
	return knowledge.NewProfile(ModelSHA256, TokenizerSHA256, ONNXRuntimeID, minimum)
}
