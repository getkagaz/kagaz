package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNoTextUtil means macOS's textutil is not on this machine. It is the
// expected condition on Linux CI and the reason this tier degrades rather than
// failing the document.
var ErrNoTextUtil = errors.New("textutil not found")

// textUtilPath is where textutil lives, and the only place it is accepted from.
//
// It ships inside macOS; it is not a tool a user installs, so there is no
// second location to search and a `textutil` earlier on $PATH is somebody
// else's program with the same name. Resolution still goes through lookPath so
// tests can simulate a machine without it, but the answer must be this path --
// the same path the doc comment and the user-facing error both promise.
const textUtilPath = "/usr/bin/textutil"

// textUtilTimeout bounds one conversion.
//
// No caller in the chain sets a deadline, and `kagaz watch` is unattended: a
// malformed `.rtf` that wedges textutil would wedge the watcher for as long as
// the daemon runs. Ollama's tier bounds itself for exactly this reason, and
// five minutes is far past any honest document's conversion.
const textUtilTimeout = 5 * time.Minute

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
	_, ok := textUtilBin()
	return ok
}

// textUtilBin resolves textutil, accepting only textUtilPath.
func textUtilBin() (string, bool) {
	p, ok := toolPath("textutil")
	if !ok || p != textUtilPath {
		return "", false
	}
	return p, true
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
// Output is capped at MaxOfficeTextBytes the same way every other tier caps,
// and capped *as it arrives*: textutil will happily convert a 400 MB manuscript,
// and buffering that whole conversion only to keep its first megabyte was
// measured at 898 MB of resident memory on a 172 MB `.rtf`.
func (t *TextUtil) Extract(ctx context.Context, path string) (Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if strings.HasPrefix(filepath.Base(path), "-") {
		// A leading dash makes the filename an option to textutil, not an
		// operand. Every caller today passes an absolute path so this cannot
		// happen, but that is a property of code two packages away, and the
		// argv of a subprocess is not a thing to secure at a distance.
		return Result{Engine: "none"}, fmt.Errorf(
			"textutil: %s: a filename beginning with %q would be read as an option, not a file; "+
				"pass it with a directory prefix such as ./%s", filepath.Base(path), "-", filepath.Base(path))
	}
	bin, ok := textUtilBin()
	if !ok {
		name := officeFormatName(ext)
		if name == "" {
			name = ext
		}
		// Name the tool, not the file. The document is fine; this machine
		// simply has no converter for it.
		return Result{Engine: "none"}, fmt.Errorf(
			"textutil: %s: %w -- reading a %s document needs macOS's /usr/bin/textutil, which is not on this machine",
			filepath.Base(path), ErrNoTextUtil, name)
	}

	ctx, cancel := context.WithTimeout(ctx, textUtilTimeout)
	defer cancel()

	// The sink *is* the buffer: bytes past the cap are never held, so the
	// process's peak memory is the cap and not the conversion.
	sink := &textSink{limit: MaxOfficeTextBytes}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-convert", "txt", "-encoding", "UTF-8", "-stdout", path)
	cmd.Stdout = sink
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Result{Engine: "none"}, fmt.Errorf(
				"textutil: %s: the conversion did not finish within %s and was stopped; "+
					"the file is very large or malformed enough to wedge textutil",
				filepath.Base(path), textUtilTimeout)
		}
		return Result{Engine: "none"}, wrapExitErr("textutil", err, stderr.String())
	}

	text := strings.TrimSpace(sink.String())
	if text == "" {
		return Result{Engine: "none"}, fmt.Errorf("textutil: %s: %w (the document carries no text)",
			filepath.Base(path), ErrNoText)
	}
	return Result{Text: text, Engine: t.Name(), Confidence: 1, Pages: 1}, nil
}

// detail is the doctor line for this runner.
func (t *TextUtil) detail() string {
	if path, ok := textUtilBin(); ok {
		return path + "; reads " + strings.Join(TextUtilExtensions, ", ")
	}
	return "textutil not found (it ships with macOS; on other systems " +
		strings.Join(TextUtilExtensions, ", ") + " cannot be read)"
}
