package ocr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// installStubHelper writes a /bin/sh stub with the given body and points
// $KAGAZ_MACHELPER at it. It stands in for the Swift binary on any platform,
// so these tests run on Linux CI.
func installStubHelper(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), HelperBinary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("writing stub helper: %v", err)
	}
	t.Setenv(HelperPathEnv, path)
}

func TestRunHelperSurfacesStructuredError(t *testing.T) {
	// The helper writes its failure payload to stdout, not stderr, and exits
	// non-zero (MacHelper/Contract.swift fail()).
	const payload = `{"contract":1,"error":"unsupported_format","message":"the file at scan.tif is not a recognizable image or PDF"}`
	installStubHelper(t, "printf '%s\\n' '"+payload+"'\nexit 1\n")

	out, err := RunHelper(context.Background(), "ocr", "scan.tif", "--json")
	if err == nil {
		t.Fatal("RunHelper() succeeded, want an error")
	}
	if len(out) == 0 {
		t.Error("RunHelper() discarded stdout on failure; the payload lives there")
	}

	var failure *HelperFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error is %T, want *HelperFailure", err)
	}
	if failure.Code != "unsupported_format" {
		t.Errorf("Code = %q, want %q", failure.Code, "unsupported_format")
	}
	if !strings.Contains(err.Error(), "unsupported_format") ||
		!strings.Contains(err.Error(), "not a recognizable image") {
		t.Errorf("error = %q, want the contract code and message", err)
	}
}

func TestRunHelperFallsBackToStderr(t *testing.T) {
	installStubHelper(t, "echo 'dyld: library not loaded' >&2\nexit 1\n")

	_, err := RunHelper(context.Background(), "ocr", "scan.tif", "--json")
	if err == nil {
		t.Fatal("RunHelper() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "dyld: library not loaded") {
		t.Errorf("error = %q, want the stderr line", err)
	}
}

func TestRunHelperSuccess(t *testing.T) {
	installStubHelper(t, "printf '%s\\n' '{\"contract\":1,\"engine\":\"vision\",\"confidence\":0,\"blocks\":[]}'\n")

	out, err := RunHelper(context.Background(), "ocr", "scan.tif", "--json")
	if err != nil {
		t.Fatalf("RunHelper() error = %v", err)
	}
	if !strings.Contains(string(out), `"engine":"vision"`) {
		t.Fatalf("stdout = %q, want the helper payload", out)
	}
}

// TestHelperFailureNamesTheBinaryThatFailed: the message used to hardcode
// kagaz-machelper, so a kagaz-machelper-mlx failure told the user to fix a
// binary that was working -- in the tier most likely to fail.
func TestHelperFailureNamesTheBinaryThatFailed(t *testing.T) {
	tests := []struct {
		name    string
		failure HelperFailure
		want    string
	}{
		{
			"an empty Binary still names the default helper",
			HelperFailure{Code: "no_text"},
			HelperBinary + ": no_text",
		},
		{
			"the MLX helper names itself",
			HelperFailure{Binary: "kagaz-machelper-mlx", Code: "model_load_failed", Message: "no weights"},
			"kagaz-machelper-mlx: model_load_failed: no weights",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.failure.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
