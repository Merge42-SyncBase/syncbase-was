package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
)

func TestProcessorRunsRealPipelineAndActivates(t *testing.T) {
	profile, _, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	run := knowledge.ClaimedRun{
		RunID:              uuid.New(),
		DocumentID:         uuid.New(),
		VersionID:          uuid.New(),
		Version:            1,
		StorageKey:         "aa/bb/original",
		ContentSHA256:      "4fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ProfileFingerprint: profile.Fingerprint,
		Fence:              7,
	}
	store := &recordingRunStore{}
	embedder := &fixtureEmbedder{}
	processor := New(
		store,
		fixtureOriginalStore{path: "/objects/original.pdf"},
		fixtureParser{pages: []knowledge.PageText{{
			PageNumber: 1,
			Text:       "비밀번호는 90일마다 변경합니다. 접근 토큰도 안전하게 보관합니다.",
		}}},
		embedder,
		profile,
	)
	if err := processor.Process(context.Background(), run); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if store.failed {
		t.Fatal("run was marked failed")
	}
	if !store.finalized || store.pageCount != 1 || store.chunkCount != 1 {
		t.Fatalf("finalization = %v pages=%d chunks=%d", store.finalized, store.pageCount, store.chunkCount)
	}
	if len(store.chunks) != 1 || len(store.chunks[0].Embedding) != knowledge.VectorDimension {
		t.Fatalf("stored chunks = %+v", store.chunks)
	}
	if len(embedder.passages) != 1 || embedder.passages[0] != store.chunks[0].Text {
		t.Fatalf("embedded passages = %q, stored text = %q", embedder.passages, store.chunks[0].Text)
	}
	wantStages := []knowledge.Stage{knowledge.StageParse, knowledge.StageChunk, knowledge.StageEmbed, knowledge.StageStore, knowledge.StageActivate}
	if len(store.stages) != len(wantStages) {
		t.Fatalf("stages = %v, want %v", store.stages, wantStages)
	}
	for index := range wantStages {
		if store.stages[index] != wantStages[index] {
			t.Fatalf("stage %d = %s, want %s", index, store.stages[index], wantStages[index])
		}
	}
}

func TestProcessorResumesValidatedParseAndChunkCheckpoints(t *testing.T) {
	profile, _, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	run := knowledge.ClaimedRun{
		RunID: uuid.New(), DocumentID: uuid.New(), VersionID: uuid.New(), Version: 1,
		StorageKey: "aa/bb/original", ContentSHA256: "4fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ProfileFingerprint: profile.Fingerprint, Fence: 7, AutomaticAttempt: 1,
	}
	store := &recordingRunStore{checkpoints: make(map[knowledge.Stage]knowledge.Checkpoint)}
	artifacts := newMemoryArtifactStore()
	parser := &countingParser{pages: []knowledge.PageText{{PageNumber: 1, Text: "복구 가능한 운영 정책입니다."}}}
	embedder := &flakyEmbedder{failure: knowledge.ErrTemporarilyUnavailable}
	processor := New(store, artifacts, parser, embedder, profile)

	if err := processor.Process(context.Background(), run); !errors.Is(err, knowledge.ErrTemporarilyUnavailable) {
		t.Fatalf("first Process error = %v, want ErrTemporarilyUnavailable", err)
	}
	run.Fence = 8
	run.AutomaticAttempt = 2
	if err := processor.Process(context.Background(), run); err != nil {
		t.Fatalf("resumed Process: %v", err)
	}
	if parser.calls != 1 {
		t.Fatalf("PDF parser calls = %d, want 1 after checkpoint resume", parser.calls)
	}
	if artifacts.pathCalls != 1 {
		t.Fatalf("original path calls = %d, want 1 after checkpoint resume", artifacts.pathCalls)
	}
	if store.checkpoints[knowledge.StageParse].OutputSHA256 == "" ||
		store.checkpoints[knowledge.StageChunk].OutputSHA256 == "" {
		t.Fatalf("missing validated checkpoints: %+v", store.checkpoints)
	}
}

func TestProcessorRecomputesSemanticallyInvalidCheckpoint(t *testing.T) {
	profile, _, err := knowledge.NewProfile(
		"ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665",
		"0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
		"onnxruntime-1.26.0",
		0.62,
	)
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	run := knowledge.ClaimedRun{
		RunID: uuid.New(), DocumentID: uuid.New(), VersionID: uuid.New(), Version: 1,
		StorageKey: "aa/bb/original", ContentSHA256: "4fe7058ff1630cefff78883aa69066d06f81af9b4daeeb7d36bd9aae9ecf95a8",
		ProfileFingerprint: profile.Fingerprint, Fence: 7, AutomaticAttempt: 1,
	}
	store := &recordingRunStore{checkpoints: make(map[knowledge.Stage]knowledge.Checkpoint)}
	artifacts := newMemoryArtifactStore()
	invalidPages := []byte("[]")
	artifactKey, err := artifacts.Put(context.Background(), invalidPages)
	if err != nil {
		t.Fatalf("Put invalid checkpoint: %v", err)
	}
	digest := sha256.Sum256(invalidPages)
	store.checkpoints[knowledge.StageParse] = knowledge.Checkpoint{
		RunID: run.RunID, Stage: knowledge.StageParse, InputSHA256: run.ContentSHA256,
		OutputSHA256: hex.EncodeToString(digest[:]), FormatVersion: checkpointFormatVersion,
		ArtifactKey: artifactKey, ArtifactSize: int64(len(invalidPages)), FencingToken: run.Fence,
	}
	parser := &countingParser{pages: []knowledge.PageText{{
		PageNumber: 1, Text: "유효한 원문을 다시 파싱합니다.",
	}}}
	processor := New(store, artifacts, parser, &fixtureEmbedder{}, profile)

	if err := processor.Process(context.Background(), run); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if parser.calls != 1 {
		t.Fatalf("PDF parser calls = %d, want 1 for invalid checkpoint", parser.calls)
	}
}

type recordingRunStore struct {
	stages      []knowledge.Stage
	chunks      []knowledge.IndexedChunk
	finalized   bool
	failed      bool
	pageCount   int
	chunkCount  int
	checkpoints map[knowledge.Stage]knowledge.Checkpoint
}

func (s *recordingRunStore) LoadCheckpoint(_ context.Context, run knowledge.ClaimedRun, stage knowledge.Stage, inputSHA256 string) (*knowledge.Checkpoint, error) {
	checkpoint, ok := s.checkpoints[stage]
	if !ok || checkpoint.InputSHA256 != inputSHA256 {
		return nil, nil
	}
	checkpoint.FencingToken = run.Fence
	return &checkpoint, nil
}

func (s *recordingRunStore) SaveCheckpoint(_ context.Context, _ knowledge.ClaimedRun, checkpoint knowledge.Checkpoint) error {
	if s.checkpoints == nil {
		s.checkpoints = make(map[knowledge.Stage]knowledge.Checkpoint)
	}
	s.checkpoints[checkpoint.Stage] = checkpoint
	return nil
}

func (s *recordingRunStore) SetStage(_ context.Context, _ uuid.UUID, _ int64, stage knowledge.Stage) error {
	s.stages = append(s.stages, stage)
	return nil
}

func (s *recordingRunStore) StoreChunks(_ context.Context, _ knowledge.ClaimedRun, chunks []knowledge.IndexedChunk) error {
	s.chunks = chunks
	return nil
}

func (s *recordingRunStore) Finalize(_ context.Context, _ knowledge.ClaimedRun, pages, chunks int) error {
	s.finalized = true
	s.pageCount = pages
	s.chunkCount = chunks
	return nil
}

func (s *recordingRunStore) Fail(_ context.Context, _ knowledge.ClaimedRun, _ knowledge.Stage, _ string) error {
	s.failed = true
	return nil
}

type fixtureOriginalStore struct{ path string }

func (s fixtureOriginalStore) Path(string) (string, error) { return s.path, nil }
func (s fixtureOriginalStore) Put(context.Context, []byte) (string, error) {
	return "checkpoint", nil
}
func (s fixtureOriginalStore) Read(context.Context, string) ([]byte, error) { return nil, nil }

type fixtureParser struct{ pages []knowledge.PageText }

func (p fixtureParser) ParseFile(context.Context, string) ([]knowledge.PageText, error) {
	return p.pages, nil
}

type countingParser struct {
	pages []knowledge.PageText
	calls int
}

func (p *countingParser) ParseFile(context.Context, string) ([]knowledge.PageText, error) {
	p.calls++
	return p.pages, nil
}

type fixtureEmbedder struct{ passages []string }

func (e *fixtureEmbedder) EmbedPassages(_ context.Context, passages []string, _ knowledge.Profile) ([][]float32, error) {
	e.passages = append([]string(nil), passages...)
	result := make([][]float32, len(passages))
	for index := range passages {
		result[index] = make([]float32, knowledge.VectorDimension)
		result[index][0] = 1
	}
	return result, nil
}

func (e *fixtureEmbedder) CountTokens(text string) (int, error) {
	return max(1, len(strings.Fields(text))), nil
}

type flakyEmbedder struct {
	failure error
	calls   int
}

func (e *flakyEmbedder) EmbedPassages(_ context.Context, passages []string, _ knowledge.Profile) ([][]float32, error) {
	e.calls++
	if e.calls == 1 {
		return nil, e.failure
	}
	result := make([][]float32, len(passages))
	for index := range passages {
		result[index] = make([]float32, knowledge.VectorDimension)
		result[index][0] = 1
	}
	return result, nil
}

func (e *flakyEmbedder) CountTokens(text string) (int, error) {
	return max(1, len(strings.Fields(text))), nil
}

type memoryArtifactStore struct {
	values    map[string][]byte
	pathCalls int
}

func newMemoryArtifactStore() *memoryArtifactStore {
	return &memoryArtifactStore{values: make(map[string][]byte)}
}

func (s *memoryArtifactStore) Path(string) (string, error) {
	s.pathCalls++
	return "/objects/original.pdf", nil
}

func (s *memoryArtifactStore) Put(_ context.Context, content []byte) (string, error) {
	key := uuid.NewString()
	s.values[key] = append([]byte(nil), content...)
	return key, nil
}

func (s *memoryArtifactStore) Read(_ context.Context, key string) ([]byte, error) {
	return append([]byte(nil), s.values[key]...), nil
}
