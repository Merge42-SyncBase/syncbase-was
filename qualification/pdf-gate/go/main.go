package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"golang.org/x/text/unicode/norm"
)

var (
	pdfiumOnce     sync.Once
	pdfiumPool     pdfium.Pool
	pdfiumInstance pdfium.Pdfium
	pdfiumInitErr  error
)

type manifest struct {
	FixtureID  string    `json:"fixture_id"`
	Iterations int       `json:"iterations"`
	Fixtures   []fixture `json:"fixtures"`
}

type fixture struct {
	ID          string   `json:"id"`
	File        string   `json:"file"`
	Expectation string   `json:"expectation"`
	PageSHA256  []string `json:"page_sha256"`
}

type fixtureResult struct {
	DocumentSHA256     string   `json:"document_sha256,omitempty"`
	ElapsedMS          int64    `json:"elapsed_ms"`
	Expectation        string   `json:"expectation"`
	Failures           []string `json:"failures"`
	File               string   `json:"file"`
	FirstMismatchText  string   `json:"first_mismatch_preview,omitempty"`
	ID                 string   `json:"id"`
	ObservedPageSHA256 []string `json:"observed_page_sha256"`
	ObservedPages      int      `json:"observed_pages"`
	Verdict            string   `json:"verdict"`
}

type evidence struct {
	Candidate             string          `json:"candidate"`
	Environment           map[string]any  `json:"environment"`
	FinishedAt            string          `json:"finished_at"`
	FixtureID             string          `json:"fixture_id"`
	FixtureManifestSHA256 string          `json:"fixture_manifest_sha256"`
	GateID                string          `json:"gate_id"`
	Iterations            int             `json:"iterations"`
	OverallVerdict        string          `json:"overall_verdict"`
	ResultSHA256          string          `json:"result_sha256,omitempty"`
	Results               []fixtureResult `json:"results"`
	SchemaVersion         int             `json:"schema_version"`
	StartedAt             string          `json:"started_at"`
	TotalElapsedMS        int64           `json:"total_elapsed_ms"`
}

func main() {
	manifestPath := flag.String("manifest", "", "fixture manifest path")
	outputPath := flag.String("output", "", "evidence output path")
	iterationOverride := flag.Int("iterations", 0, "override iteration count")
	flag.Parse()
	if *manifestPath == "" || *outputPath == "" {
		fatal(errors.New("-manifest and -output are required"))
	}
	defer closePDFium()

	manifestBytes, err := os.ReadFile(*manifestPath)
	if err != nil {
		fatal(err)
	}
	var input manifest
	if err := json.Unmarshal(manifestBytes, &input); err != nil {
		fatal(err)
	}
	iterations := input.Iterations
	if *iterationOverride > 0 {
		iterations = *iterationOverride
	}
	if iterations < 1 {
		fatal(errors.New("iterations must be positive"))
	}

	started := time.Now()
	results := make([]fixtureResult, 0, len(input.Fixtures))
	overall := "PASS"
	root := directoryOf(*manifestPath)
	for _, item := range input.Fixtures {
		result := runFixture(root, item, iterations)
		results = append(results, result)
		if result.Verdict != "PASS" {
			overall = "FAIL"
		}
	}

	output := evidence{
		Candidate:             "go-pdfium-webassembly-v1.19.6",
		Environment:           environment(),
		FinishedAt:            time.Now().UTC().Format(time.RFC3339Nano),
		FixtureID:             input.FixtureID,
		FixtureManifestSHA256: hashBytes(manifestBytes),
		GateID:                "GQ-2",
		Iterations:            iterations,
		OverallVerdict:        overall,
		Results:               results,
		SchemaVersion:         1,
		StartedAt:             started.UTC().Format(time.RFC3339Nano),
		TotalElapsedMS:        time.Since(started).Milliseconds(),
	}
	canonical, err := json.Marshal(output)
	if err != nil {
		fatal(err)
	}
	output.ResultSHA256 = hashBytes(canonical)
	formatted, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(directoryOf(*outputPath), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*outputPath, append(formatted, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("%s %s\n", overall, *outputPath)
	if overall != "PASS" {
		os.Exit(1)
	}
}

func runFixture(root string, item fixture, iterations int) fixtureResult {
	started := time.Now()
	failures := make([]string, 0)
	documentHashes := make([]string, 0, iterations)
	firstObservedPageHashes := make([]string, 0)
	firstMismatchText := ""
	observedPages := -1
	path := root + string(os.PathSeparator) + item.File

	for iteration := 1; iteration <= iterations; iteration++ {
		pages, err := extractPages(path)
		if err != nil {
			if item.Expectation != "INVALID_INPUT" {
				failures = append(failures, fmt.Sprintf("iteration %d: %v", iteration, err))
			}
			continue
		}
		observedPages = len(pages)
		if item.Expectation == "INVALID_INPUT" {
			failures = append(failures, fmt.Sprintf("iteration %d: invalid fixture exposed text", iteration))
			continue
		}
		pageHashes := make([]string, len(pages))
		for index, page := range pages {
			pageHashes[index] = hashBytes([]byte(page))
		}
		if iteration == 1 {
			firstObservedPageHashes = pageHashes
		}
		if !equalStrings(pageHashes, item.PageSHA256) {
			failures = append(failures, fmt.Sprintf("iteration %d: page text hash mismatch", iteration))
			if firstMismatchText == "" {
				firstMismatchText = preview(pages)
			}
		}
		documentHashes = append(documentHashes, documentHash(pages))
	}

	if item.Expectation != "INVALID_INPUT" && distinctCount(documentHashes) != 1 {
		failures = append(failures, "document hash changed across iterations")
	}
	result := fixtureResult{
		ElapsedMS:          time.Since(started).Milliseconds(),
		Expectation:        item.Expectation,
		Failures:           failures,
		File:               item.File,
		FirstMismatchText:  firstMismatchText,
		ID:                 item.ID,
		ObservedPageSHA256: firstObservedPageHashes,
		ObservedPages:      observedPages,
		Verdict:            "PASS",
	}
	if len(documentHashes) > 0 {
		result.DocumentSHA256 = documentHashes[0]
	}
	if len(failures) > 0 {
		result.Verdict = "FAIL"
	}
	return result
}

func preview(pages []string) string {
	joined := strings.Join(pages, "\n---PAGE---\n")
	const limit = 1000
	if len(joined) <= limit {
		return joined
	}
	return joined[:limit]
}

func extractPages(path string) (pages []string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("parser panic: %v", recovered)
		}
	}()
	engine, err := getPDFium()
	if err != nil {
		return nil, err
	}
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read PDF: %w", err)
	}
	document, err := engine.OpenDocument(&requests.OpenDocument{File: &file})
	if err != nil {
		return nil, fmt.Errorf("open PDF: %w", err)
	}
	defer func() {
		_, closeErr := engine.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: document.Document})
		if err == nil && closeErr != nil {
			err = fmt.Errorf("close PDF: %w", closeErr)
		}
	}()
	count, err := engine.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: document.Document})
	if err != nil {
		return nil, fmt.Errorf("count PDF pages: %w", err)
	}
	pageCount := count.PageCount
	if pageCount < 1 {
		return nil, errors.New("PDF has no pages")
	}
	pages = make([]string, 0, pageCount)
	for pageIndex := 0; pageIndex < pageCount; pageIndex++ {
		text, pageErr := engine.GetPageText(&requests.GetPageText{
			Page: requests.Page{ByIndex: &requests.PageByIndex{
				Document: document.Document,
				Index:    pageIndex,
			}},
		})
		if pageErr != nil {
			return nil, fmt.Errorf("page %d: %w", pageIndex+1, pageErr)
		}
		pages = append(pages, normalize(text.Text))
	}
	allBlank := true
	for _, page := range pages {
		if strings.TrimSpace(page) != "" {
			allBlank = false
			break
		}
	}
	if allBlank {
		return nil, errors.New("PDF has no extractable text")
	}
	return pages, nil
}

func getPDFium() (pdfium.Pdfium, error) {
	pdfiumOnce.Do(func() {
		pdfiumPool, pdfiumInitErr = webassembly.Init(webassembly.Config{
			MinIdle:      1,
			MaxIdle:      1,
			MaxTotal:     1,
			ReuseWorkers: true,
		})
		if pdfiumInitErr != nil {
			return
		}
		pdfiumInstance, pdfiumInitErr = pdfiumPool.GetInstance(30 * time.Second)
	})
	if pdfiumInitErr != nil {
		return nil, fmt.Errorf("initialize PDFium WebAssembly: %w", pdfiumInitErr)
	}
	return pdfiumInstance, nil
}

func closePDFium() {
	if pdfiumInstance != nil {
		_ = pdfiumInstance.Close()
	}
	if pdfiumPool != nil {
		_ = pdfiumPool.Close()
	}
}

func normalize(input string) string {
	normalized := norm.NFC.String(strings.ReplaceAll(strings.ReplaceAll(input, "\r\n", "\n"), "\r", "\n"))
	normalized = strings.ReplaceAll(normalized, "\x00", "")
	lines := strings.Split(normalized, "\n")
	clean := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			clean = append(clean, line)
		}
	}
	return strings.Join(clean, "\n")
}

func documentHash(pages []string) string {
	hash := sha256.New()
	for index, page := range pages {
		fmt.Fprintf(hash, "%d%c%s\n", index+1, byte(0), page)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func environment() map[string]any {
	info, _ := debug.ReadBuildInfo()
	goVersion := runtime.Version()
	if info != nil && info.GoVersion != "" {
		goVersion = info.GoVersion
	}
	return map[string]any{
		"arch":               runtime.GOARCH,
		"go":                 goVersion,
		"goroutines":         runtime.NumGoroutine(),
		"logical_processors": runtime.NumCPU(),
		"os":                 runtime.GOOS,
	}
}

func directoryOf(path string) string {
	index := strings.LastIndex(path, string(os.PathSeparator))
	if index < 0 {
		return "."
	}
	if index == 0 {
		return string(os.PathSeparator)
	}
	return path[:index]
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func distinctCount(values []string) int {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		unique[value] = struct{}{}
	}
	return len(unique)
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
