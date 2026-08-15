package ocr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// withoutHelper arranges for HelperPath to find nothing: no environment
// override, no sibling binary, nothing on PATH. It skips the test if a real
// helper happens to be installed in the Homebrew prefix on this machine.
func withoutHelper(t *testing.T) {
	t.Helper()
	for _, dir := range helperPrefixes {
		if isExecutableFile(filepath.Join(dir, HelperBinary)) {
			t.Skipf("%s is installed in %s; this test needs it absent", HelperBinary, dir)
		}
	}
	t.Setenv(HelperPathEnv, "")
	stubLookPath(t, nil)
	stubExecutable(t, filepath.Join(t.TempDir(), "kagaz"))
}

func stubExecutable(t *testing.T, path string) {
	t.Helper()
	orig := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = orig })
}

// writeFakeHelper creates an executable stub file and returns its path.
func writeFakeHelper(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, HelperBinary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake helper: %v", err)
	}
	return path
}

func TestHelperPathSearchOrder(t *testing.T) {
	t.Run("environment override wins", func(t *testing.T) {
		withoutHelper(t)
		want := writeFakeHelper(t, t.TempDir())
		t.Setenv(HelperPathEnv, want)

		got, ok := HelperPath()
		if !ok || got != want {
			t.Fatalf("HelperPath() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("bad environment override does not fall through", func(t *testing.T) {
		withoutHelper(t)
		t.Setenv(HelperPathEnv, filepath.Join(t.TempDir(), "missing"))

		if got, ok := HelperPath(); ok {
			t.Fatalf("HelperPath() = (%q, true), want not found", got)
		}
	})

	t.Run("sibling of the running executable", func(t *testing.T) {
		withoutHelper(t)
		dir := t.TempDir()
		want := writeFakeHelper(t, dir)
		stubExecutable(t, filepath.Join(dir, "kagaz"))

		got, ok := HelperPath()
		if !ok || got != want {
			t.Fatalf("HelperPath() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("PATH", func(t *testing.T) {
		withoutHelper(t)
		want := writeFakeHelper(t, t.TempDir())
		stubLookPath(t, map[string]string{HelperBinary: want})

		got, ok := HelperPath()
		if !ok || got != want {
			t.Fatalf("HelperPath() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("absent everywhere", func(t *testing.T) {
		withoutHelper(t)
		if got, ok := HelperPath(); ok {
			t.Fatalf("HelperPath() = (%q, true), want not found", got)
		}
	})
}

func TestHelperPrefixesAreAppleSiliconOnly(t *testing.T) {
	for _, dir := range helperPrefixes {
		if dir == "/usr/local/bin" {
			t.Fatal("Intel Homebrew prefix must not be probed: Kagaz is Apple-silicon only")
		}
	}
}

func TestRunHelperWithoutHelper(t *testing.T) {
	withoutHelper(t)
	if _, err := RunHelper(context.Background()); !errors.Is(err, ErrNoHelper) {
		t.Fatalf("RunHelper() error = %v, want ErrNoHelper", err)
	}
}
