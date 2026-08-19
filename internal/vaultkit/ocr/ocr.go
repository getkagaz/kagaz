// Package ocr turns a document into text. It prefers the cheapest source that
// works: a plain-text file read directly (no tooling at all), an existing PDF
// text layer via poppler's pdftotext (milliseconds),
// then Apple's Vision framework through kagaz-machelper (~1s/page, no model
// weights), and only on request a local Ollama vision model.
//
// Everything here is on-device. The Ollama runner refuses any endpoint that is
// not localhost.
package ocr

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// Result is extracted text plus its provenance.
type Result struct {
	Text       string
	Engine     string // "plaintext" | "office" | "textutil" | "legacyoffice" | "pdftotext" | "vision" | "ollama:<model>" | "none"
	Confidence float64
	Pages      int
}

// Runner is one text-extraction backend.
type Runner interface {
	Name() string
	Available() bool
	Extract(ctx context.Context, path string) (Result, error)
}

// ErrNoText means the runner produced nothing usable.
var ErrNoText = errors.New("no text extracted")

// Extractor picks a runner per document.
type Extractor struct {
	cfg    *config.Config
	engine string // "" = auto; otherwise force "pdftotext" | "vision" | "ollama"

	Text     *PlainText
	Office   *Office
	TextUtil *TextUtil
	Legacy   *LegacyOffice
	PDF      *PDFToText
	Vision   *Vision
	Ollama   *Ollama
}

// NewExtractor builds the extraction pipeline for a vault. engine forces a
// specific backend; empty means automatic selection.
func NewExtractor(cfg *config.Config, engine string) *Extractor {
	return &Extractor{
		cfg:      cfg,
		engine:   engine,
		Text:     &PlainText{},
		Office:   &Office{},
		TextUtil: &TextUtil{},
		Legacy:   &LegacyOffice{},
		PDF:      &PDFToText{},
		Vision:   &Vision{Languages: cfg.OCR.VisionLanguages},
		Ollama: &Ollama{
			Endpoint: cfg.OCR.Ollama.Endpoint,
			Model:    cfg.OCR.Ollama.Model,
			Enabled:  cfg.OCR.Ollama.Enabled,
		},
	}
}

// Extract returns the document's text, choosing a backend automatically unless
// one was forced.
func (e *Extractor) Extract(ctx context.Context, path string) (Result, error) {
	switch e.engine {
	case "plaintext":
		return e.Text.Extract(ctx, path)
	case "office":
		return e.Office.Extract(ctx, path)
	case "textutil":
		return e.TextUtil.Extract(ctx, path)
	case "legacyoffice":
		return e.Legacy.Extract(ctx, path)
	case "pdftotext":
		return e.PDF.Extract(ctx, path)
	case "vision":
		return e.Vision.Extract(ctx, path)
	case "ollama":
		return e.Ollama.Extract(ctx, path)
	}

	ext := strings.ToLower(filepath.Ext(path))
	var firstErr error

	// A text file is its own text layer. This runs first and returns
	// unconditionally for the extensions it claims: there is no OCR tier that
	// could do better on a .txt, and falling through to Vision would burn a
	// second per page to read back what os.ReadFile already had.
	if e.Text.Handles(path) {
		return e.Text.Extract(ctx, path)
	}

	// An Office document is likewise returned unconditionally. An OOXML file has
	// no PDF text layer and is not an image, so neither the PDF tier nor OCR can
	// do anything with it.
	if e.Office.Handles(path) {
		return e.Office.Extract(ctx, path)
	}

	// The same reasoning claims the legacy binary formats, and for the same
	// reason it must happen before the PDF and Vision tiers: there is no image
	// to OCR and no text layer to find, so a `.doc` or `.xls` that fell through
	// here would spend a second per page proving it.
	//
	// `.doc`/`.rtf`/`.odt` go to textutil, which macOS ships and Linux does
	// not; `.xls`/`.ppt` are parsed in-process because no system tool reads
	// them. Both claim their extensions whether or not the backing tool exists,
	// so an absent textutil produces an error that names the missing tool
	// rather than a generic "no text".
	if e.TextUtil.Handles(path) {
		return e.TextUtil.Extract(ctx, path)
	}
	if e.Legacy.Handles(path) {
		return e.Legacy.Extract(ctx, path)
	}

	if ext == ".pdf" && e.PDF.Available() {
		res, err := e.PDF.Extract(ctx, path)
		if err == nil && HasUsableTextLayer(res.Text, res.Pages) {
			return res, nil
		}
		if err != nil {
			firstErr = err
		}
	}

	if e.Vision.Available() {
		res, err := e.Vision.Extract(ctx, path)
		if err == nil && strings.TrimSpace(res.Text) != "" {
			return res, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Available() is the whole gate: it returns false unless the vault opted
	// in (ocr.ollama.enabled defaults to off), so an omitted key never reaches
	// a daemon even where one is running with the model loaded.
	if e.Ollama.Available() {
		res, err := e.Ollama.Extract(ctx, path)
		if err == nil && strings.TrimSpace(res.Text) != "" {
			return res, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// A PDF with a thin text layer still beats nothing.
	if ext == ".pdf" && e.PDF.Available() {
		if res, err := e.PDF.Extract(ctx, path); err == nil && strings.TrimSpace(res.Text) != "" {
			return res, nil
		}
	}

	if firstErr != nil {
		return Result{Engine: "none"}, firstErr
	}
	return Result{Engine: "none"}, ErrNoText
}

// HasUsableTextLayer decides whether a PDF's embedded text is good enough to
// skip OCR.
//
// Scanned PDFs frequently carry a *thin* text layer -- a few stray glyphs, page
// furniture, or mojibake from a bad encoding -- so a simple non-empty check
// sends real scans down the wrong path. This wants enough characters per page,
// a healthy proportion of letters, and evidence of actual words.
func HasUsableTextLayer(text string, pages int) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if pages < 1 {
		pages = 1
	}

	var letters, digits, spaces, other int
	for _, r := range trimmed {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.IsDigit(r):
			digits++
		case unicode.IsSpace(r):
			spaces++
		case unicode.IsPrint(r):
			other++
		default:
			other++
		}
	}
	total := letters + digits + spaces + other
	if total == 0 {
		return false
	}

	// At least ~120 characters of substance per page. A title page plus a scan
	// of the real content should not pass.
	if (letters+digits)/pages < 120 {
		return false
	}
	// Mojibake and glyph soup are mostly non-letters.
	if float64(letters)/float64(total) < 0.35 {
		return false
	}
	// Real prose has multi-letter words; extraction failures give single glyphs.
	words := strings.Fields(trimmed)
	if len(words) < 20 {
		return false
	}
	long := 0
	for _, w := range words {
		if len([]rune(w)) >= 3 {
			long++
		}
	}
	return float64(long)/float64(len(words)) >= 0.4
}

// Describe reports which backends are usable, for `kagaz doctor`.
func (e *Extractor) Describe() []Status {
	return []Status{
		{Name: e.Text.Name(), Available: e.Text.Available(), Detail: e.Text.detail()},
		{Name: e.Office.Name(), Available: e.Office.Available(), Detail: e.Office.detail()},
		{Name: e.TextUtil.Name(), Available: e.TextUtil.Available(), Detail: e.TextUtil.detail()},
		{Name: e.Legacy.Name(), Available: e.Legacy.Available(), Detail: e.Legacy.detail()},
		{Name: e.PDF.Name(), Available: e.PDF.Available(), Detail: e.PDF.detail()},
		{Name: e.Vision.Name(), Available: e.Vision.Available(), Detail: e.Vision.detail()},
		{Name: e.Ollama.Name(), Available: e.Ollama.Available(), Detail: e.Ollama.detail()},
	}
}

// Status is one backend's availability, for doctor output.
type Status struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}

// lookPath is a thin wrapper so tests can stub tool discovery.
var lookPath = exec.LookPath

func toolPath(name string) (string, bool) {
	p, err := lookPath(name)
	if err != nil {
		return "", false
	}
	return p, true
}

func wrapExitErr(tool string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return fmt.Errorf("%s: %w: %s", tool, err, firstLine(stderr))
	}
	return fmt.Errorf("%s: %w", tool, err)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
