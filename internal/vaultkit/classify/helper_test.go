package classify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withoutMLXHelper arranges for MLXHelperPath to find nothing. It skips when a
// real helper happens to be installed in the Homebrew prefix on this machine.
func withoutMLXHelper(t *testing.T) {
	t.Helper()
	for _, dir := range mlxHelperPrefixes {
		if isExecutableFile(filepath.Join(dir, MLXHelperBinary)) {
			t.Skipf("%s is installed in %s; this test needs it absent", MLXHelperBinary, dir)
		}
	}
	t.Setenv(MLXHelperPathEnv, "")
	stubLookPath(t, nil)
	stubExecutable(t, filepath.Join(t.TempDir(), "kagaz"))
}

func stubExecutable(t *testing.T, path string) {
	t.Helper()
	orig := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = orig })
}

func stubLookPath(t *testing.T, table map[string]string) {
	t.Helper()
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if p, ok := table[name]; ok {
			return p, nil
		}
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { lookPath = orig })
}

func writeFakeMLXHelper(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, MLXHelperBinary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake helper: %v", err)
	}
	return path
}

func TestMLXHelperPathSearchOrder(t *testing.T) {
	t.Run("environment override wins", func(t *testing.T) {
		withoutMLXHelper(t)
		want := writeFakeMLXHelper(t, t.TempDir())
		t.Setenv(MLXHelperPathEnv, want)

		got, ok := MLXHelperPath()
		if !ok || got != want {
			t.Fatalf("MLXHelperPath() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("bad environment override does not fall through", func(t *testing.T) {
		withoutMLXHelper(t)
		t.Setenv(MLXHelperPathEnv, filepath.Join(t.TempDir(), "missing"))
		if got, ok := MLXHelperPath(); ok {
			t.Fatalf("MLXHelperPath() = (%q, true), want not found", got)
		}
	})

	t.Run("sibling of the running executable", func(t *testing.T) {
		withoutMLXHelper(t)
		dir := t.TempDir()
		want := writeFakeMLXHelper(t, dir)
		stubExecutable(t, filepath.Join(dir, "kagaz"))

		got, ok := MLXHelperPath()
		if !ok || got != want {
			t.Fatalf("MLXHelperPath() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("PATH", func(t *testing.T) {
		withoutMLXHelper(t)
		want := writeFakeMLXHelper(t, t.TempDir())
		stubLookPath(t, map[string]string{MLXHelperBinary: want})

		got, ok := MLXHelperPath()
		if !ok || got != want {
			t.Fatalf("MLXHelperPath() = (%q, %v), want (%q, true)", got, ok, want)
		}
	})

	t.Run("absent everywhere", func(t *testing.T) {
		withoutMLXHelper(t)
		if got, ok := MLXHelperPath(); ok {
			t.Fatalf("MLXHelperPath() = (%q, true), want not found", got)
		}
	})
}

func TestMLXHelperPrefixesAreAppleSiliconOnly(t *testing.T) {
	for _, dir := range mlxHelperPrefixes {
		if dir == "/usr/local/bin" {
			t.Fatal("Intel Homebrew prefix must not be probed: Kagaz is Apple-silicon only")
		}
	}
}

// TestExecHelperPipesStdinAndReturnsStdoutOnFailure exercises the real
// exec path against a shell stub -- not against any Kagaz helper, which does
// not exist on CI. It is skipped on platforms without /bin/sh.
func TestExecHelperPipesStdinAndReturnsStdoutOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()

	echoStub := filepath.Join(dir, "echo-stub")
	if err := os.WriteFile(echoStub, []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	out, err := execHelper(context.Background(), echoStub, nil, "hello stdin")
	if err != nil {
		t.Fatalf("execHelper: %v", err)
	}
	if string(out) != "hello stdin" {
		t.Fatalf("stdout = %q, want the piped stdin", out)
	}

	failStub := filepath.Join(dir, "fail-stub")
	script := "#!/bin/sh\nprintf '%s' '{\"contract\":1,\"error\":\"model_unavailable\",\"message\":\"no weights\"}'\nexit 3\n"
	if err := os.WriteFile(failStub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	out, err = execHelper(context.Background(), failStub, nil, "")
	if err == nil {
		t.Fatal("expected an error from a non-zero exit")
	}
	if !strings.Contains(err.Error(), "model_unavailable") {
		t.Fatalf("error = %q, want the structured error code", err)
	}
	if !strings.Contains(string(out), "model_unavailable") {
		t.Fatalf("stdout = %q, want it returned even on failure", out)
	}
}

func TestErrNoHelperIsWrappable(t *testing.T) {
	err := (&Apple{locate: missing}).classifyErr()
	if !errors.Is(err, ErrNoHelper) {
		t.Fatalf("error = %v, want ErrNoHelper", err)
	}
}

// classifyErr is a tiny helper so the wrapping test reads clearly.
func (a *Apple) classifyErr() error {
	_, err := a.Classify(context.Background(), Request{})
	return err
}
