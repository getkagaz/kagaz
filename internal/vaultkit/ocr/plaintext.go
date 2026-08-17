package ocr

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxPlainTextBytes caps how much of a plain-text file is read.
//
// It is deliberately larger than sidecar.MaxText (256 KiB, what is stored) and
// classify's maxText (20000 runes, what a model is shown), because both of
// those truncate downstream and this is only the read budget. A document that
// needs more than a megabyte of leading text to be classifiable is not one
// more bytes would help.
const MaxPlainTextBytes = 1 << 20

// PlainTextExtensions are the extensions PlainText handles, lowercased and
// including the dot.
var PlainTextExtensions = []string{".txt", ".md", ".markdown", ".text"}

// PlainText reads a text file as its own text layer.
//
// It exists because there was no path at all for a `.txt` or `.md` file:
// ingest reported "no text extractor for .txt on this machine", which named a
// machine problem no machine can fix, for a format that needs no tooling
// whatsoever. Kagaz's own fixture vault is made of `.txt` files.
//
// This runner is always available: it depends on nothing but the standard
// library, so it is the one tier that cannot degrade (Global Constraint 9 has
// nothing to do here).
type PlainText struct{}

// Name identifies the runner in Result.Engine and doctor output.
func (p *PlainText) Name() string { return "plaintext" }

// Available is always true: reading a file needs no external tool.
func (p *PlainText) Available() bool { return true }

// Handles reports whether path looks like a plain-text document by extension.
// Extension, not sniffing: a `.pdf` whose bytes happen to be printable is
// still a PDF, and guessing would silently steal documents from the runners
// that understand them.
func (p *PlainText) Handles(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, want := range PlainTextExtensions {
		if ext == want {
			return true
		}
	}
	return false
}

// Extract reads up to MaxPlainTextBytes of path.
//
// Binary content is refused rather than returned as glyph soup: a NUL byte or
// invalid UTF-8 means the extension lied, and a wrong answer here would be
// classified, scored and written to a sidecar as fact.
func (p *PlainText) Extract(_ context.Context, path string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("plaintext: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, MaxPlainTextBytes))
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("plaintext: %s: %w", filepath.Base(path), err)
	}
	text := string(data)
	if strings.ContainsRune(text, 0) {
		return Result{Engine: "none"}, fmt.Errorf("plaintext: %s: %w (the file is binary, despite its extension)",
			filepath.Base(path), ErrNoText)
	}
	// A truncated read can split a rune; drop the partial tail rather than
	// calling the whole file invalid.
	if len(data) == MaxPlainTextBytes {
		text = strings.ToValidUTF8(text, "")
	} else if !utf8.ValidString(text) {
		return Result{Engine: "none"}, fmt.Errorf("plaintext: %s: %w (not valid UTF-8)",
			filepath.Base(path), ErrNoText)
	}
	if strings.TrimSpace(text) == "" {
		return Result{Engine: "none"}, fmt.Errorf("plaintext: %s: %w", filepath.Base(path), ErrNoText)
	}

	return Result{Text: text, Engine: p.Name(), Confidence: 1, Pages: 1}, nil
}

// detail is the doctor line for this runner.
func (p *PlainText) detail() string {
	return "built in; reads " + strings.Join(PlainTextExtensions, ", ") + " with no external tool"
}
