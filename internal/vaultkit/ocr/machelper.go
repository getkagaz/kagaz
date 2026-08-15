package ocr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNoHelper means kagaz-machelper is not installed (or $KAGAZ_MACHELPER points
// at something that is not executable). It is an expected condition on Linux and
// on a Mac without the optional helper, not a bug.
var ErrNoHelper = errors.New("kagaz-machelper not found")

// HelperBinary is the name of the optional Swift sidecar that exposes macOS
// system frameworks (Vision, NaturalLanguage, Foundation Models) to the Go
// core. It is always optional: every caller must degrade gracefully when it is
// absent, which is the normal case on Linux CI.
const HelperBinary = "kagaz-machelper"

// HelperPathEnv overrides helper discovery with an explicit path. It exists for
// development builds and for tests; a packaged install never needs it.
const HelperPathEnv = "KAGAZ_MACHELPER"

// helperPrefixes are the fixed directories probed last, after $KAGAZ_MACHELPER,
// the running executable's own directory and $PATH.
//
// Only the Apple-silicon Homebrew prefix is listed. Kagaz is Apple-silicon
// only, so the Intel prefix /usr/local/bin is deliberately not probed.
var helperPrefixes = []string{"/opt/homebrew/bin"}

// osExecutable is a seam so tests can control the "next to the running binary"
// probe.
var osExecutable = os.Executable

// HelperPath resolves the kagaz-machelper binary, returning its absolute-ish
// path and whether it was found. It is the single helper-discovery
// implementation in the repository: other packages (classify) must call this
// rather than repeating the search order.
//
// Search order, first hit wins:
//
//  1. $KAGAZ_MACHELPER, if it names an executable file;
//  2. the directory of the running executable, so a locally built kagaz finds
//     its sibling helper without installing anything;
//  3. $PATH;
//  4. the Homebrew prefix /opt/homebrew/bin.
//
// HelperPath holds no OCR-specific state and never runs the helper.
func HelperPath() (string, bool) {
	if p := os.Getenv(HelperPathEnv); p != "" {
		if isExecutableFile(p) {
			return p, true
		}
		// An explicit override that does not resolve is a configuration
		// mistake, not a reason to silently fall back to a different binary.
		return "", false
	}

	if exe, err := osExecutable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), HelperBinary); isExecutableFile(p) {
			return p, true
		}
	}

	if p, ok := toolPath(HelperBinary); ok {
		return p, true
	}

	for _, dir := range helperPrefixes {
		if p := filepath.Join(dir, HelperBinary); isExecutableFile(p) {
			return p, true
		}
	}
	return "", false
}

// HelperFailure is a helper run that exited non-zero. The helper writes its
// structured failure payload to *stdout* -- `{"contract":1,"error":"<code>",
// "message":"..."}` -- so Code and Message carry the machine-readable reason
// (`file_not_found`, `unsupported_format`, `no_text`, `backend_unavailable`,
// ...) that callers switch on to decide how to degrade. Code and Message are
// empty when the output could not be decoded as a contract payload.
type HelperFailure struct {
	// Code is the contract's "error" value, e.g. "unsupported_format".
	Code string
	// Message is the contract's human-readable "message" value.
	Message string
	// Stdout is the raw output, kept for diagnostics.
	Stdout []byte
	// Err is the underlying *exec.ExitError.
	Err error
}

// Error renders the code and message when the helper supplied them, and falls
// back to the exit status plus stderr otherwise.
func (e *HelperFailure) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("%s: %s: %s", HelperBinary, e.Code, e.Message)
	case e.Code != "":
		return fmt.Sprintf("%s: %s", HelperBinary, e.Code)
	case e.Message != "":
		return fmt.Sprintf("%s: %v: %s", HelperBinary, e.Err, e.Message)
	default:
		return fmt.Sprintf("%s: %v", HelperBinary, e.Err)
	}
}

// Unwrap exposes the underlying exec error to errors.Is/As.
func (e *HelperFailure) Unwrap() error { return e.Err }

// helperErrorPayload is the contract's failure shape (§ "Error shape").
type helperErrorPayload struct {
	Contract int    `json:"contract"`
	Error    string `json:"error"`
	Message  string `json:"message"`
}

// RunHelper executes kagaz-machelper with the given arguments and returns its
// standard output.
//
// Stdout is returned even when the helper exits non-zero, because that is where
// the structured failure payload is written; the error is then a *HelperFailure
// carrying the decoded Code and Message. Stderr is folded into the error when
// the helper produced no decodable payload, so nothing is ever lost.
//
// It returns ErrNoHelper, never a panic, when the helper is not installed; the
// caller decides how to degrade. This is the single invocation path for the
// helper and is shared with the classify package.
func RunHelper(ctx context.Context, args ...string) ([]byte, error) {
	path, ok := HelperPath()
	if !ok {
		return nil, ErrNoHelper
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), helperFailure(err, stdout.Bytes(), stderr.String())
	}
	return stdout.Bytes(), nil
}

// helperFailure builds the richest error the helper's output allows.
func helperFailure(runErr error, stdout []byte, stderr string) error {
	var payload helperErrorPayload
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &payload); err == nil && payload.Error != "" {
		return &HelperFailure{
			Code:    payload.Error,
			Message: payload.Message,
			Stdout:  stdout,
			Err:     runErr,
		}
	}
	return &HelperFailure{
		Message: firstLine(strings.TrimSpace(stderr)),
		Stdout:  stdout,
		Err:     runErr,
	}
}

// isExecutableFile reports whether path is a regular file with an execute bit.
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}
