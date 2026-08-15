package ocr

import (
	"os"
	"path/filepath"
)

// FindHelper resolves an optional Kagaz helper binary by name, using the one
// search order this repository has for helpers. It exists so that packages
// which ship a *different* helper binary -- classify's kagaz-machelper-mlx --
// reuse the search order instead of re-implementing it and drifting from it.
//
// binary is the executable's name; pathEnv is the environment variable that
// overrides discovery for development builds and tests.
//
// Search order, first hit wins:
//
//  1. $<pathEnv>, if it names an executable file;
//  2. the directory of the running executable, so a locally built kagaz finds
//     its sibling helper without installing anything;
//  3. $PATH;
//  4. the Homebrew prefix /opt/homebrew/bin.
//
// HelperPath() is exactly FindHelper(HelperBinary, HelperPathEnv); it is kept
// as its own function because it is the documented entry point for the Swift
// helper, and collapsing the two would mean editing machelper.go.
//
// FindHelper never runs the binary and holds no state.
func FindHelper(binary, pathEnv string) (string, bool) {
	if p := os.Getenv(pathEnv); p != "" {
		if isExecutableFile(p) {
			return p, true
		}
		// An explicit override that does not resolve is a configuration
		// mistake, not a reason to silently run a different binary.
		return "", false
	}

	if exe, err := osExecutable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), binary); isExecutableFile(p) {
			return p, true
		}
	}

	if p, ok := toolPath(binary); ok {
		return p, true
	}

	for _, dir := range helperPrefixes {
		if p := filepath.Join(dir, binary); isExecutableFile(p) {
			return p, true
		}
	}
	return "", false
}
