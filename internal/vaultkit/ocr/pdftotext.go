package ocr

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// ErrNoPDFToText means poppler's pdftotext is not installed. Callers degrade to
// another runner rather than failing.
var ErrNoPDFToText = errors.New("pdftotext not found")

// PDFToText extracts an existing PDF text layer with poppler's pdftotext. It is
// by far the cheapest runner -- milliseconds, no model weights -- but it only
// works when the PDF already carries text. Scans need Vision or Ollama.
type PDFToText struct{}

// Name identifies the runner in Result.Engine and doctor output.
func (p *PDFToText) Name() string { return "pdftotext" }

// Available reports whether poppler's pdftotext is installed.
func (p *PDFToText) Available() bool {
	_, ok := toolPath("pdftotext")
	return ok
}

// detail explains, for `kagaz doctor`, either where the binary was found or why
// the runner is unusable.
func (p *PDFToText) detail() string {
	if path, ok := toolPath("pdftotext"); ok {
		return path
	}
	return "pdftotext not found (install poppler: brew install poppler)"
}

// Extract runs `pdftotext -layout -enc UTF-8 <path> -` and returns the embedded
// text layer. Page count comes from pdfinfo when it is installed, and otherwise
// from the form feeds pdftotext writes between pages.
//
// Extract does not judge the quality of what it found; callers use
// HasUsableTextLayer for that.
func (p *PDFToText) Extract(ctx context.Context, path string) (Result, error) {
	bin, ok := toolPath("pdftotext")
	if !ok {
		return Result{Engine: "none"}, ErrNoPDFToText
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-layout", "-enc", "UTF-8", path, "-")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Result{Engine: "none"}, wrapExitErr("pdftotext", err, stderr.String())
	}

	text := stdout.String()
	pages := pdfPageCount(ctx, path)
	if pages < 1 {
		pages = pageCountFromFormFeeds(text)
	}

	return Result{
		Text:       strings.TrimSpace(strings.ReplaceAll(text, "\f", "\n")),
		Engine:     p.Name(),
		Confidence: 1, // a real text layer is exact, not a guess
		Pages:      pages,
	}, nil
}

// pdfPageCount asks pdfinfo for the page count, returning 0 when pdfinfo is
// missing or unhelpful so the caller can fall back to counting form feeds.
func pdfPageCount(ctx context.Context, path string) int {
	bin, ok := toolPath("pdfinfo")
	if !ok {
		return 0
	}
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, path)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return 0
	}
	return parsePDFInfoPages(stdout.String())
}

// parsePDFInfoPages pulls the "Pages:" line out of pdfinfo's key/value output.
func parsePDFInfoPages(out string) int {
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "Pages") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 1 {
			return 0
		}
		return n
	}
	return 0
}

// pageCountFromFormFeeds derives a page count from pdftotext output, which
// terminates every page with a form feed. Empty output is zero pages; anything
// else is at least one.
func pageCountFromFormFeeds(text string) int {
	if strings.TrimSpace(text) == "" {
		return 0
	}
	n := strings.Count(text, "\f")
	if n < 1 {
		return 1
	}
	return n
}
