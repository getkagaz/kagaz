package ocr

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// RunHelper executes kagaz-machelper with the given arguments and returns its
// standard output. Standard error is folded into the returned error so a
// helper failure reports why rather than just an exit status.
//
// It returns an error, never a panic, when the helper is not installed; the
// caller decides how to degrade.
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
		return nil, wrapExitErr(HelperBinary, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// isExecutableFile reports whether path is a regular file with an execute bit.
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}
