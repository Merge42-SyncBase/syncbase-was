// Package processing coordinates the fenced document-processing pipeline.
package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	"github.com/google/uuid"
)

type runStore interface {
	SetStage(context.Context, uuid.UUID, int64, knowledge.Stage) error
	LoadCheckpoint(context.Context, knowledge.ClaimedRun, knowledge.Stage, string) (*knowledge.Checkpoint, error)
	SaveCheckpoint(context.Context, knowledge.ClaimedRun, knowledge.Checkpoint) error
	StoreChunks(context.Context, knowledge.ClaimedRun, []knowledge.IndexedChunk) error
	Finalize(context.Context, knowledge.ClaimedRun, int, int) error
	Fail(context.Context, knowledge.ClaimedRun, knowledge.Stage, string) error
}

type artifactStore interface {
	Path(string) (string, error)
	Put(context.Context, []byte) (string, error)
	Read(context.Context, string) ([]byte, error)
}

type pdfParser interface {
	ParseFile(context.Context, string) ([]knowledge.PageText, error)
}

type embedder interface {
	EmbedPassages(context.Context, []string, knowledge.Profile) ([][]float32, error)
	CountTokens(string) (int, error)
}

// Processor advances one claimed run from parsing through active-version publication.
type Processor struct {
	runs      runStore
	artifacts artifactStore
	parser    pdfParser
	embedder  embedder
	profile   knowledge.Profile
}

// New returns a processor that coordinates a fenced parse-to-activate run.
func New(
	runs runStore,
	artifacts artifactStore,
	parser pdfParser,
	embedder embedder,
	profile knowledge.Profile,
) *Processor {
	return &Processor{
		runs:      runs,
		artifacts: artifacts,
		parser:    parser,
		embedder:  embedder,
		profile:   profile,
	}
}

// Process executes a claimed run and records recoverable stage checkpoints.
func (p *Processor) Process(ctx context.Context, run knowledge.ClaimedRun) error {
	stage := knowledge.StageMetadata
	fail := func(cause error) error {
		if errors.Is(cause, knowledge.ErrStaleFence) || errors.Is(cause, context.Canceled) ||
			errors.Is(ctx.Err(), context.Canceled) {
			return cause
		}
		failureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := p.runs.Fail(failureContext, run, stage, failureCode(cause)); err != nil {
			return errors.Join(cause, fmt.Errorf("mark processing run failed: %w", err))
		}
		return cause
	}
	if run.ProfileFingerprint != p.profile.Fingerprint {
		return fail(knowledge.ErrProfileMismatch)
	}

	stage = knowledge.StageParse
	if err := p.runs.SetStage(ctx, run.RunID, run.Fence, stage); err != nil {
		return fail(err)
	}
	pages := []knowledge.PageText(nil)
	parseDigest, resumed, err := p.loadCheckpoint(ctx, run, stage, run.ContentSHA256, &pages)
	if err != nil {
		return fail(err)
	}
	if !resumed {
		path, err := p.artifacts.Path(run.StorageKey)
		if err != nil {
			return fail(fmt.Errorf("original missing: %w", err))
		}
		pages, err = p.parser.ParseFile(ctx, path)
		if err != nil {
			return fail(err)
		}
		parseDigest, err = p.saveCheckpoint(ctx, run, stage, run.ContentSHA256, pages)
		if err != nil {
			return fail(err)
		}
	}

	stage = knowledge.StageChunk
	if err := p.runs.SetStage(ctx, run.RunID, run.Fence, stage); err != nil {
		return fail(err)
	}
	chunks := []knowledge.Chunk(nil)
	chunkDigest, resumed, err := p.loadCheckpoint(ctx, run, stage, parseDigest, &chunks)
	if err != nil {
		return fail(err)
	}
	if !resumed {
		chunks, err = ChunkPagesWithProfile(pages, p.embedder.CountTokens, p.profile)
		if err != nil {
			return fail(err)
		}
		if len(chunks) == 0 {
			return fail(fmt.Errorf("no searchable text: %w", knowledge.ErrInvalidPDF))
		}
		chunkDigest, err = p.saveCheckpoint(ctx, run, stage, parseDigest, chunks)
		if err != nil {
			return fail(err)
		}
	}
	if len(chunks) == 0 {
		return fail(fmt.Errorf("no searchable text: %w", knowledge.ErrInvalidPDF))
	}

	stage = knowledge.StageEmbed
	if err := p.runs.SetStage(ctx, run.RunID, run.Fence, stage); err != nil {
		return fail(err)
	}
	indexed := []knowledge.IndexedChunk(nil)
	_, resumed, err = p.loadCheckpoint(ctx, run, stage, chunkDigest, &indexed)
	if err != nil {
		return fail(err)
	}
	if !resumed {
		passages := make([]string, len(chunks))
		for index, chunk := range chunks {
			passages[index] = chunk.Text
		}
		vectors, err := p.embedder.EmbedPassages(ctx, passages, p.profile)
		if err != nil {
			return fail(err)
		}
		if len(vectors) != len(chunks) {
			return fail(knowledge.ErrProfileMismatch)
		}
		indexed = make([]knowledge.IndexedChunk, len(chunks))
		for index, chunk := range chunks {
			if len(vectors[index]) != knowledge.VectorDimension {
				return fail(knowledge.ErrProfileMismatch)
			}
			indexed[index] = knowledge.IndexedChunk{
				Chunk: chunk, Snippet: snippet(chunk.Text), Embedding: vectors[index],
			}
		}
		if _, err := p.saveCheckpoint(ctx, run, stage, chunkDigest, indexed); err != nil {
			return fail(err)
		}
	}

	stage = knowledge.StageStore
	if err := p.runs.SetStage(ctx, run.RunID, run.Fence, stage); err != nil {
		return fail(err)
	}
	if err := p.runs.StoreChunks(ctx, run, indexed); err != nil {
		return fail(err)
	}

	stage = knowledge.StageActivate
	if err := p.runs.SetStage(ctx, run.RunID, run.Fence, stage); err != nil {
		return fail(err)
	}
	if err := p.runs.Finalize(ctx, run, len(pages), len(indexed)); err != nil {
		return fail(err)
	}
	return nil
}

func failureCode(err error) string {
	switch {
	case errors.Is(err, knowledge.ErrInvalidPDF), errors.Is(err, knowledge.ErrInvalidArgument):
		return "INVALID_INPUT"
	case errors.Is(err, knowledge.ErrProfileMismatch):
		return "PROFILE_MISMATCH"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, knowledge.ErrTemporarilyUnavailable):
		return "TEMPORARILY_UNAVAILABLE"
	default:
		return "INTERNAL"
	}
}

const checkpointFormatVersion = "syncbase-json-v1"

func (p *Processor) saveCheckpoint(
	ctx context.Context,
	run knowledge.ClaimedRun,
	stage knowledge.Stage,
	inputSHA256 string,
	value any,
) (string, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode %s checkpoint: %w", stage, err)
	}
	digest := sha256.Sum256(content)
	outputSHA256 := hex.EncodeToString(digest[:])
	key, err := p.artifacts.Put(ctx, content)
	if err != nil {
		return "", fmt.Errorf("store %s checkpoint artifact: %w", stage, err)
	}
	checkpoint := knowledge.Checkpoint{
		RunID: run.RunID, Stage: stage, InputSHA256: inputSHA256,
		OutputSHA256: outputSHA256, FormatVersion: checkpointFormatVersion,
		ArtifactKey: key, ArtifactSize: int64(len(content)), FencingToken: run.Fence,
	}
	if err := p.runs.SaveCheckpoint(ctx, run, checkpoint); err != nil {
		return "", fmt.Errorf("save %s checkpoint: %w", stage, err)
	}
	return outputSHA256, nil
}

func (p *Processor) loadCheckpoint(
	ctx context.Context,
	run knowledge.ClaimedRun,
	stage knowledge.Stage,
	inputSHA256 string,
	destination any,
) (string, bool, error) {
	checkpoint, err := p.runs.LoadCheckpoint(ctx, run, stage, inputSHA256)
	if err != nil {
		return "", false, fmt.Errorf("load %s checkpoint: %w", stage, err)
	}
	if checkpoint == nil || checkpoint.FormatVersion != checkpointFormatVersion {
		return "", false, nil
	}
	content, err := p.artifacts.Read(ctx, checkpoint.ArtifactKey)
	if err != nil || int64(len(content)) != checkpoint.ArtifactSize {
		return "", false, nil
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != checkpoint.OutputSHA256 {
		return "", false, nil
	}
	if err := json.Unmarshal(content, destination); err != nil {
		return "", false, nil
	}
	if !validCheckpointPayload(destination) {
		return "", false, nil
	}
	return checkpoint.OutputSHA256, true, nil
}

func validCheckpointPayload(destination any) bool {
	switch value := destination.(type) {
	case *[]knowledge.PageText:
		if len(*value) == 0 {
			return false
		}
		hasText := false
		for index, page := range *value {
			if page.PageNumber != index+1 {
				return false
			}
			hasText = hasText || strings.TrimSpace(page.Text) != ""
		}
		return hasText
	case *[]knowledge.Chunk:
		return validChunks(*value)
	case *[]knowledge.IndexedChunk:
		if len(*value) == 0 {
			return false
		}
		chunks := make([]knowledge.Chunk, len(*value))
		for index, indexed := range *value {
			chunks[index] = indexed.Chunk
			if strings.TrimSpace(indexed.Snippet) == "" || len(indexed.Embedding) != knowledge.VectorDimension {
				return false
			}
			for _, component := range indexed.Embedding {
				if math.IsNaN(float64(component)) || math.IsInf(float64(component), 0) {
					return false
				}
			}
		}
		return validChunks(chunks)
	default:
		return false
	}
}

func validChunks(chunks []knowledge.Chunk) bool {
	if len(chunks) == 0 {
		return false
	}
	lastPage := 0
	for index, chunk := range chunks {
		if chunk.Index != index || chunk.PageNumber < 1 || chunk.PageNumber < lastPage ||
			strings.TrimSpace(chunk.Text) == "" {
			return false
		}
		lastPage = chunk.PageNumber
	}
	return true
}

func snippet(text string) string {
	compact := strings.Join(strings.Fields(text), " ")
	runes := []rune(compact)
	if len(runes) > 400 {
		runes = runes[:400]
	}
	return string(runes)
}
