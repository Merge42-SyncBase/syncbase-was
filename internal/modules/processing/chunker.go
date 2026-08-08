// Package processing implements SyncBase's document-processing module.
package processing

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
)

// TokenCounter returns the number of tokens produced by the pinned processing
// tokenizer for one passage.
type TokenCounter func(string) (int, error)

// ChunkPagesWithCounter applies the production token contract while preserving
// page boundaries. Forced splits retain up to 64 tokenizer tokens of overlap.
func ChunkPagesWithCounter(pages []knowledge.PageText, counter TokenCounter) ([]knowledge.Chunk, error) {
	if counter == nil {
		return nil, knowledge.ErrInvalidArgument
	}
	chunks := make([]knowledge.Chunk, 0)
	for _, page := range pages {
		text := strings.TrimSpace(page.Text)
		if text == "" {
			continue
		}
		pageStart := len(chunks)
		current := ""
		flushCurrent := func() {
			if strings.TrimSpace(current) != "" {
				chunks = append(chunks, knowledge.Chunk{PageNumber: page.PageNumber, Text: current})
			}
			current = ""
		}
		for _, sentence := range sentences(text) {
			sentenceTokens, err := counter(sentence)
			if err != nil || sentenceTokens < 1 {
				return nil, fmt.Errorf("count sentence tokens: %w", firstError(err, knowledge.ErrInvalidArgument))
			}
			if sentenceTokens > maxUnits {
				flushCurrent()
				parts, err := forcedTokenSplit(sentence, counter)
				if err != nil {
					return nil, err
				}
				for _, part := range parts {
					chunks = append(chunks, knowledge.Chunk{PageNumber: page.PageNumber, Text: part})
				}
				continue
			}
			candidate := sentence
			if current != "" {
				candidate = current + " " + sentence
			}
			candidateTokens, err := counter(candidate)
			if err != nil {
				return nil, fmt.Errorf("count candidate tokens: %w", err)
			}
			if current != "" && candidateTokens > targetUnits {
				flushCurrent()
				current = sentence
			} else {
				current = candidate
			}
		}
		flushCurrent()
		if err := mergeTokenTail(&chunks, pageStart, counter); err != nil {
			return nil, err
		}
	}
	for index := range chunks {
		chunks[index].Index = index
	}
	return chunks, nil
}

func forcedTokenSplit(text string, counter TokenCounter) ([]string, error) {
	runes := []rune(text)
	parts := make([]string, 0)
	for start := 0; start < len(runes); {
		end, err := tokenBoundedEnd(runes, start, counter, maxUnits)
		if err != nil {
			return nil, err
		}
		if end <= start {
			return nil, knowledge.ErrInvalidArgument
		}
		if end < len(runes) {
			for boundary := end; boundary > start; boundary-- {
				if unicode.IsSpace(runes[boundary-1]) {
					end = boundary - 1
					break
				}
			}
		}
		part := strings.TrimSpace(string(runes[start:end]))
		if part != "" {
			parts = append(parts, part)
		}
		if end >= len(runes) {
			break
		}
		next, err := tokenOverlapStart(runes, start, end, counter, forcedOverlapUnits)
		if err != nil {
			return nil, err
		}
		if next <= start {
			next = start + 1
		}
		start = next
		for start < len(runes) && unicode.IsSpace(runes[start]) {
			start++
		}
	}
	return parts, nil
}

func tokenBoundedEnd(runes []rune, start int, counter TokenCounter, limit int) (int, error) {
	low, high := start+1, len(runes)
	best := start
	for low <= high {
		middle := low + (high-low)/2
		count, err := counter(strings.TrimSpace(string(runes[start:middle])))
		if err != nil {
			return 0, err
		}
		if count <= limit {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return best, nil
}

func tokenOverlapStart(runes []rune, start, end int, counter TokenCounter, limit int) (int, error) {
	low, high := start+1, end
	best := end
	for low <= high {
		middle := low + (high-low)/2
		count, err := counter(strings.TrimSpace(string(runes[middle:end])))
		if err != nil {
			return 0, err
		}
		if count <= limit {
			best = middle
			high = middle - 1
		} else {
			low = middle + 1
		}
	}
	for best < end && unicode.IsSpace(runes[best]) {
		best++
	}
	for best > start+1 && !unicode.IsSpace(runes[best-1]) {
		best--
	}
	return best, nil
}

func mergeTokenTail(chunks *[]knowledge.Chunk, pageStart int, counter TokenCounter) error {
	values := *chunks
	if len(values)-pageStart < 2 {
		return nil
	}
	tail := values[len(values)-1]
	previous := values[len(values)-2]
	tailTokens, err := counter(tail.Text)
	if err != nil {
		return err
	}
	combined := previous.Text + " " + tail.Text
	combinedTokens, err := counter(combined)
	if err != nil {
		return err
	}
	if tailTokens < minUnits && combinedTokens <= maxUnits {
		values[len(values)-2].Text = combined
		*chunks = values[:len(values)-1]
	}
	return nil
}

func firstError(primary, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

const (
	targetUnits        = 384
	maxUnits           = 480
	minUnits           = 80
	forcedOverlapUnits = 64
)

// ChunkPages creates deterministic page-bounded chunks.
func ChunkPages(pages []knowledge.PageText) []knowledge.Chunk {
	chunks := make([]knowledge.Chunk, 0)
	for _, page := range pages {
		text := strings.TrimSpace(page.Text)
		if text == "" {
			continue
		}
		var current strings.Builder
		for _, sentence := range sentences(text) {
			if units(sentence) > maxUnits {
				flush(&chunks, page.PageNumber, &current)
				forcedSplit(&chunks, page.PageNumber, sentence)
				continue
			}
			candidate := sentence
			if current.Len() > 0 {
				candidate = current.String() + " " + sentence
			}
			if current.Len() > 0 && units(candidate) > targetUnits {
				flush(&chunks, page.PageNumber, &current)
			}
			if current.Len() > 0 {
				current.WriteByte(' ')
			}
			current.WriteString(sentence)
		}
		flush(&chunks, page.PageNumber, &current)
		mergeShortTail(&chunks, page.PageNumber)
	}
	for index := range chunks {
		chunks[index].Index = index
	}
	return chunks
}

func sentences(text string) []string {
	runes := []rune(text)
	output := make([]string, 0)
	start := 0
	for index, r := range runes {
		if r != '.' && r != '!' && r != '?' && r != '。' && r != '\n' {
			continue
		}
		part := strings.TrimSpace(string(runes[start : index+1]))
		if part != "" {
			output = append(output, part)
		}
		start = index + 1
	}
	if tail := strings.TrimSpace(string(runes[start:])); tail != "" {
		output = append(output, tail)
	}
	if len(output) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return output
}

func forcedSplit(chunks *[]knowledge.Chunk, page int, text string) {
	runes := []rune(text)
	maxRunes := maxUnits * 4
	overlapRunes := forcedOverlapUnits * 4
	for start := 0; start < len(runes); {
		end := min(len(runes), start+maxRunes)
		if end < len(runes) {
			for boundary := end; boundary > start+maxRunes/2; boundary-- {
				if runes[boundary-1] == ' ' {
					end = boundary - 1
					break
				}
			}
		}
		if part := strings.TrimSpace(string(runes[start:end])); part != "" {
			*chunks = append(*chunks, knowledge.Chunk{PageNumber: page, Text: part})
		}
		if end == len(runes) {
			break
		}
		start = max(start+1, end-overlapRunes)
	}
}

func flush(chunks *[]knowledge.Chunk, page int, current *strings.Builder) {
	if current.Len() == 0 {
		return
	}
	*chunks = append(*chunks, knowledge.Chunk{PageNumber: page, Text: current.String()})
	current.Reset()
}

func mergeShortTail(chunks *[]knowledge.Chunk, page int) {
	values := *chunks
	if len(values) < 2 {
		return
	}
	tail := values[len(values)-1]
	previous := values[len(values)-2]
	combined := previous.Text + " " + tail.Text
	if tail.PageNumber == page && previous.PageNumber == page && units(tail.Text) < minUnits && units(combined) <= maxUnits {
		values[len(values)-2].Text = combined
		*chunks = values[:len(values)-1]
	}
}

func units(text string) int {
	value := (utf8.RuneCountInString(text) + 3) / 4
	return max(1, value)
}
