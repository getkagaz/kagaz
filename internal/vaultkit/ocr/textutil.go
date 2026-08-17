package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNoTextUtil means macOS's textutil is not on this machine. It is the
// expected condition on Linux CI and the reason this tier degrades rather than
// failing the document.
var ErrNoTextUtil = errors.New("textutil not found")

// TextUtilExtensions are the rich-text formats textutil converts, lowercased
// and including the dot.
//
// The list stops where textutil's own `-convert txt` reliably stops. `.pages`
// and `.key` are not here: they are iWork bundles, not formats textutil reads.
var TextUtilExtensions = []string{".doc", ".rtf", ".rtfd", ".odt", ".wordml"}

// textUtilNames give each extension the name a user would recognise, so a
// failure names a format rather than a mystery extension.
var textUtilNames = map[string]string{
	".doc":    "Word 97-2003",
	".rtf":    "Rich Text Format",
	".rtfd":   "Rich Text Format Directory",
	".odt":    "OpenDocument Text",
	".wordml": "Word 2003 XML",
}

// TextUtil extracts text by shelling out to macOS's /usr/bin/textutil.
//
// It exists because `.doc` and `.rtf` are still ordinary things to find in a
// folder of real documents, and macOS already ships a converter that reads them
// correctly -- including the Word binary format's piece table, which is exactly
// the kind of parser Kagaz should not be writing. This follows the pattern
// pdftotext and kagaz-machelper already established: no dependency, no vendored
// parser, and `kagaz doctor` reports whether it is there.
//
// It is macOS-only and therefore optional (Global Constraint 9): Available
// returns false wherever textutil is absent, and every failure names the
// missing tool rather than blaming the file.
type TextUtil struct{}

// Name identifies the runner in Result.Engine and doctor output.
func (t *TextUtil) Name() string { return "textutil" }

// Available reports whether macOS's textutil is installed. It is false on Linux
// and on any machine without it; callers degrade rather than failing.
func (t *TextUtil) Available() bool {
	_, ok := toolPath("textutil")
	return ok
}

// Handles reports whether path is a format textutil converts.
//
// Handles is deliberately independent of Available: a `.doc` is a `.doc` on
// Linux too, and claiming it here is what lets Extract explain that the tool is
// missing instead of letting the file fall through to OCR and die as a generic
// "no text" a page of Vision later.
func (t *TextUtil) Handles(path string) bool {
	return extIn(path, TextUtilExtensions)
}

// Extract runs `textutil -convert txt -encoding UTF-8 -stdout <path>`.
//
// Output is capped at MaxOfficeTextBytes the same way every other tier caps:
// textutil will happily convert a 400 MB manuscript, and nothing downstream
// wants more than the leading megabyte.
func (t *TextUtil) Extract(ctx context.Context, path string) (Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	bin, ok := toolPath("textutil")
	if !ok {
		name := textUtilNames[ext]
		if name == "" {
			name = ext
		}
		// Name the tool, not the file. The document is fine; this machine
		// simply has no converter for it.
		return Result{Engine: "none"}, fmt.Errorf(
			"textutil: %s: %w -- reading a %s document needs macOS's /usr/bin/textutil, which is not on this machine",
			filepath.Base(path), ErrNoTextUtil, name)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-convert", "txt", "-encoding", "UTF-8", "-stdout", path)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Result{Engine: "none"}, wrapExitErr("textutil", err, stderr.String())
	}

	sink := &textSink{limit: MaxOfficeTextBytes}
	sink.write(stdout.String())
	text := strings.TrimSpace(sink.String())
	if text == "" {
		return Result{Engine: "none"}, fmt.Errorf("textutil: %s: %w (the document carries no text)",
			filepath.Base(path), ErrNoText)
	}
	return Result{Text: text, Engine: t.Name(), Confidence: 1, Pages: 1}, nil
}

// detail is the doctor line for this runner.
func (t *TextUtil) detail() string {
	if path, ok := toolPath("textutil"); ok {
		return path + "; reads " + strings.Join(TextUtilExtensions, ", ")
	}
	return "textutil not found (it ships with macOS; on other systems " +
		strings.Join(TextUtilExtensions, ", ") + " cannot be read)"
}
