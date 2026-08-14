// Package pdf adapts the qualified PDFium WebAssembly engine to SyncBase's page-text contract.
package pdf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Merge42-SyncBase/syncbase-was/internal/modules/knowledge"
	pdfiumapi "github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"golang.org/x/text/unicode/norm"
)

// Parser owns a bounded pool of PDFium WebAssembly instances.
type Parser struct {
	pool     pdfiumapi.Pool
	instance pdfiumapi.Pdfium
	mu       sync.Mutex
}

// New initializes the qualified PDFium WebAssembly parser.
func New(ctx context.Context) (*Parser, error) {
	pool, err := webassembly.Init(webassembly.Config{
		Context:      ctx,
		MinIdle:      1,
		MaxIdle:      1,
		MaxTotal:     1,
		ReuseWorkers: true,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize PDFium WebAssembly: %w", err)
	}
	instance, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("acquire PDFium instance: %w", err)
	}
	return &Parser{pool: pool, instance: instance}, nil
}

// Close releases the parser pool and its WebAssembly runtime.
func (p *Parser) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var result error
	if p.instance != nil {
		result = p.instance.Close()
		p.instance = nil
	}
	if p.pool != nil {
		if err := p.pool.Close(); result == nil {
			result = err
		}
		p.pool = nil
	}
	return result
}

// Ready reports whether the qualified parser runtime is initialized.
func (p *Parser) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pool == nil || p.instance == nil {
		return errors.New("PDF parser is closed")
	}
	return nil
}

// ParseFile extracts normalized text from each page of a local PDF.
func (p *Parser) ParseFile(ctx context.Context, path string) ([]knowledge.PageText, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read PDF: %w", err)
	}
	defer file.Close()
	data, err := readPDFContent(file, knowledge.MaxUploadBytes)
	if err != nil {
		return nil, err
	}
	return p.Parse(ctx, data)
}

func readPDFContent(source io.Reader, maximumSize int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(source, maximumSize+1))
	if err != nil {
		return nil, fmt.Errorf("read PDF: %w", err)
	}
	if int64(len(data)) > maximumSize {
		return nil, fmt.Errorf("%w: size", knowledge.ErrInvalidPDF)
	}
	return data, nil
}

// Parse extracts normalized, page-scoped text from one supported PDF.
func (p *Parser) Parse(ctx context.Context, data []byte) (pages []knowledge.PageText, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.instance == nil {
		return nil, errors.New("PDF parser is closed")
	}
	if len(data) < 1 || len(data) > knowledge.MaxUploadBytes {
		return nil, fmt.Errorf("%w: size", knowledge.ErrInvalidPDF)
	}
	document, err := p.instance.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return nil, fmt.Errorf("%w: open", knowledge.ErrInvalidPDF)
	}
	defer func() {
		_, closeErr := p.instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: document.Document})
		if err == nil && closeErr != nil {
			err = fmt.Errorf("close PDF: %w", closeErr)
		}
	}()
	count, err := p.instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: document.Document})
	if err != nil {
		return nil, fmt.Errorf("%w: page count", knowledge.ErrInvalidPDF)
	}
	if count.PageCount < 1 || count.PageCount > knowledge.MaxPDFPages {
		return nil, fmt.Errorf("%w: page count", knowledge.ErrInvalidPDF)
	}

	pages = make([]knowledge.PageText, 0, count.PageCount)
	anyText := false
	for pageIndex := 0; pageIndex < count.PageCount; pageIndex++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, pageErr := p.instance.GetPageText(&requests.GetPageText{
			Page: requests.Page{ByIndex: &requests.PageByIndex{
				Document: document.Document,
				Index:    pageIndex,
			}},
		})
		if pageErr != nil {
			return nil, fmt.Errorf("%w: page %d", knowledge.ErrInvalidPDF, pageIndex+1)
		}
		text := normalize(response.Text)
		anyText = anyText || text != ""
		pageNumber := pageIndex + 1
		pages = append(pages, knowledge.PageText{PageNumber: pageNumber, Text: text})
	}
	if !anyText {
		return nil, fmt.Errorf("%w: no text", knowledge.ErrInvalidPDF)
	}
	return pages, nil
}

// TextSHA256 returns the stable digest used by parser qualification tests.
func TextSHA256(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
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
