package ocr

import (
	"os"
	"path/filepath"
	"testing"
)

// otherHelperBinary stands in for a second helper binary (classify ships
// kagaz-machelper-mlx) so FindHelper is tested on a name that is not
// HelperBinary.
const otherHelperBinary = "kagaz-machelper-mlx"

// otherHelperEnv is that binary's override variable.
const otherHelperEnv = "KAGAZ_MACHELPER_MLX"

// withoutOtherHelper arranges for FindHelper(otherHelperBinary, ...) to find
// nothing, skipping if a real one is installed in the Homebrew prefix.
func withoutOtherHelper(t *testing.T) {
	t.Helper()
	for _, dir := range helperPrefixes {
		if isExecutableFile(filepath.Join(dir, otherHelperBinary)) {
			t.Skipf("%s is installed in %s; this test needs it absent", otherHelperBinary, dir)
		}
	}
	t.Setenv(otherHelperEnv, "")
	stubLookPath(t, nil)
	stubExecutable(t, filepath.Join(t.TempDir(), "kagaz"))
}

// writeFakeBinary creates an executable stub with the given name.
func writeFakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return path
}

func TestFindHelperSearchOrder(t *testing.T) {
	t.Run("environment override wins", func(t *testing.T) {
		withoutOtherHelper(t)
		want := writeFakeBinary(t, t.TempDir(), otherHelperBinary)
		t.Setenv(otherHelperEnv, want)

		got, ok := FindHelper(otherHelperBinary, otherHelperEnv)
		if !ok || got != want {
			t.Fatalf("FindHelper() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("bad environment override does not fall through", func(t *testing.T) {
		withoutOtherHelper(t)
		t.Setenv(otherHelperEnv, filepath.Join(t.TempDir(), "missing"))
		if got, ok := FindHelper(otherHelperBinary, otherHelperEnv); ok {
			t.Fatalf("FindHelper() = (%q, true), want not found", got)
		}
	})

	t.Run("sibling of the running executable", func(t *testing.T) {
		withoutOtherHelper(t)
		dir := t.TempDir()
		want := writeFakeBinary(t, dir, otherHelperBinary)
		stubExecutable(t, filepath.Join(dir, "kagaz"))

		got, ok := FindHelper(otherHelperBinary, otherHelperEnv)
		if !ok || got != want {
			t.Fatalf("FindHelper() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("PATH", func(t *testing.T) {
		withoutOtherHelper(t)
		want := writeFakeBinary(t, t.TempDir(), otherHelperBinary)
		stubLookPath(t, map[string]string{otherHelperBinary: want})

		got, ok := FindHelper(otherHelperBinary, otherHelperEnv)
		if !ok || got != want {
			t.Fatalf("FindHelper() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("absent everywhere", func(t *testing.T) {
		withoutOtherHelper(t)
		if got, ok := FindHelper(otherHelperBinary, otherHelperEnv); ok {
			t.Fatalf("FindHelper() = (%q, true), want not found", got)
		}
	})

	t.Run("does not find a differently named helper", func(t *testing.T) {
		withoutOtherHelper(t)
		dir := t.TempDir()
		writeFakeBinary(t, dir, HelperBinary) // the Swift helper, not the MLX one
		stubExecutable(t, filepath.Join(dir, "kagaz"))

		if got, ok := FindHelper(otherHelperBinary, otherHelperEnv); ok {
			t.Fatalf("FindHelper() = (%q, true), want not found: names must not be interchangeable", got)
		}
	})
}

// TestFindHelperMatchesHelperPath pins the equivalence documented on
// FindHelper, so the two cannot drift apart unnoticed.
func TestFindHelperMatchesHelperPath(t *testing.T) {
	withoutHelper(t)
	dir := t.TempDir()
	want := writeFakeBinary(t, dir, HelperBinary)
	stubExecutable(t, filepath.Join(dir, "kagaz"))

	viaHelperPath, okA := HelperPath()
	viaFindHelper, okB := FindHelper(HelperBinary, HelperPathEnv)
	if okA != okB || viaHelperPath != viaFindHelper || viaFindHelper != want {
		t.Fatalf("HelperPath() = (%q, %v), FindHelper() = (%q, %v), want both (%q, true)",
			viaHelperPath, okA, viaFindHelper, okB, want)
	}
}
