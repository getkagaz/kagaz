package classify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
)

// skipIfMLXHelperInstalled skips when a real MLX helper is in the Homebrew
// prefix, which would make a "not found" expectation wrong on that machine.
func skipIfMLXHelperInstalled(t *testing.T) {
	t.Helper()
	if fi, err := os.Stat(filepath.Join("/opt/homebrew/bin", MLXHelperBinary)); err == nil && !fi.IsDir() {
		t.Skipf("%s is installed in /opt/homebrew/bin; this test needs it absent", MLXHelperBinary)
	}
}

// TestMLXHelperPathUsesSharedDiscovery checks the binding between the
// classify-side constants and ocr.FindHelper, which owns the search order.
// The order itself is tested once, in ocr's discovery_test.go.
func TestMLXHelperPathUsesSharedDiscovery(t *testing.T) {
	skipIfMLXHelperInstalled(t)

	t.Run("environment override", func(t *testing.T) {
		dir := t.TempDir()
		want := filepath.Join(dir, MLXHelperBinary)
		if err := os.WriteFile(want, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("writing fake helper: %v", err)
		}
		t.Setenv(MLXHelperPathEnv, want)

		got, ok := MLXHelperPath()
		if !ok || got != want {
			t.Fatalf("MLXHelperPath() = (%q, %v), want (%q, true)", got, ok, want)
		}
		viaOCR, okOCR := ocr.FindHelper(MLXHelperBinary, MLXHelperPathEnv)
		if viaOCR != got || okOCR != ok {
			t.Fatalf("MLXHelperPath() = %q, ocr.FindHelper() = %q; they must be the same lookup", got, viaOCR)
		}
	})

	t.Run("bad override does not fall through", func(t *testing.T) {
		skipIfMLXHelperInstalled(t)
		t.Setenv(MLXHelperPathEnv, filepath.Join(t.TempDir(), "missing"))
		if got, ok := MLXHelperPath(); ok {
			t.Fatalf("MLXHelperPath() = (%q, true), want not found", got)
		}
	})
}

// TestExecHelperPipesStdinAndReturnsStdoutOnFailure exercises the real exec
// path against a shell stub -- not against any Kagaz helper, which does not
// exist on CI. It is skipped on platforms without a POSIX shell.
func TestExecHelperPipesStdinAndReturnsStdoutOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()

	echoStub := writeShellStub(t, dir, "echo-stub", "cat\n")
	out, err := execHelper(context.Background(), echoStub, nil, "hello stdin")
	if err != nil {
		t.Fatalf("execHelper: %v", err)
	}
	if string(out) != "hello stdin" {
		t.Fatalf("stdout = %q, want the piped stdin", out)
	}

	failStub := writeShellStub(t, dir, "fail-stub",
		"printf '%s' '{\"contract\":1,\"error\":\"model_unavailable\",\"message\":\"no weights\"}'\nexit 3\n")
	out, err = execHelper(context.Background(), failStub, nil, "")
	if err == nil {
		t.Fatal("expected an error from a non-zero exit")
	}
	var failure *ocr.HelperFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error is %T, want *ocr.HelperFailure", err)
	}
	if failure.Code != "model_unavailable" || failure.Message != "no weights" {
		t.Fatalf("failure = %+v, want the structured code and message", failure)
	}
	if !strings.Contains(string(out), "model_unavailable") {
		t.Fatalf("stdout = %q, want it returned even on failure", out)
	}
}

// TestExecHelperTimesOutDespiteOrphanedGrandchild is IMPORTANT 3.
//
// The stub spawns a background child that inherits stdout and sleeps far past
// the deadline. exec.CommandContext kills only the direct child, and cmd.Wait
// blocks until every writer to the stdout pipe closes it -- so without
// cmd.WaitDelay this call never returns and ingest stalls with no timeout.
func TestExecHelperTimesOutDespiteOrphanedGrandchild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	stub := writeShellStub(t, t.TempDir(), "orphan-stub", "sleep 30 &\nsleep 30\n")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := execHelper(ctx, stub, nil, ""); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		if !isTimeout(err) {
			t.Fatalf("error = %v, want a timeout", err)
		}
		var failure *ocr.HelperFailure
		if !errors.As(err, &failure) || failure.Code != CodeTimeout {
			t.Fatalf("error = %v (%T), want an *ocr.HelperFailure with code %q", err, err, CodeTimeout)
		}
		// WaitDelay allows a short grace period; anything near the stub's own
		// 30s sleep means Wait was blocked on the inherited pipe.
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("execHelper took %v; it waited on the orphaned grandchild's pipe", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("execHelper hung past its deadline: cmd.WaitDelay is not doing its job")
	}
}

// TestBoundedBufferCapsRetainedOutput checks a chatty helper cannot make us
// buffer without limit, and that it is never handed a short write.
func TestBoundedBufferCapsRetainedOutput(t *testing.T) {
	b := &boundedBuffer{limit: 10}
	n, err := b.Write([]byte("0123456789abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("Write() = (%d, %v), want (16, nil): a short write would EPIPE the child", n, err)
	}
	if got := b.String(); got != "0123456789" {
		t.Fatalf("retained %q, want the first 10 bytes only", got)
	}
	if n, err := b.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("Write() after the limit = (%d, %v), want (4, nil)", n, err)
	}
	if b.buf.Len() != 10 {
		t.Fatalf("buffered %d bytes, want the limit of 10", b.buf.Len())
	}
}

func TestClassifyErrorsUseTheSharedSentinel(t *testing.T) {
	if _, err := (&Apple{locate: missing}).Classify(context.Background(), Request{}); !errors.Is(err, ocr.ErrNoHelper) {
		t.Fatalf("apple error = %v, want ocr.ErrNoHelper", err)
	}
	if _, err := (&MLX{Model: "m", locate: missing}).Classify(context.Background(), Request{}); !errors.Is(err, ocr.ErrNoHelper) {
		t.Fatalf("mlx error = %v, want ocr.ErrNoHelper", err)
	}
}

// writeShellStub writes an executable /bin/sh script and returns its path.
func writeShellStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	return path
}
