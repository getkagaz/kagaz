package classify

import (
	"context"
	"fmt"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
)

// mlxClassifyTimeout is more generous than the Apple tier: an MLX model loads
// several gigabytes of weights on first use.
const mlxClassifyTimeout = 2 * time.Minute

// MLX runs a local quantised model through kagaz-machelper-mlx. It speaks the
// same versioned contract as the Apple backend but is a separate binary and
// needs weights on disk, fetched by `kagaz model pull --engine mlx`.
//
// MLX must not be copied after first use: it caches its probe behind a mutex.
// Hold it by pointer, as Chain does.
type MLX struct {
	// Model is the Hugging Face repo path, e.g.
	// "mlx-community/Qwen2.5-3B-Instruct-4bit".
	Model string
	// Timeout bounds one classification; zero means mlxClassifyTimeout.
	Timeout time.Duration

	// run is a test seam. Nil means execHelper.
	run helperRunner
	// locate is a test seam over discovery. Nil means MLXHelperPath.
	locate func() (string, bool)

	probeCache probeCache
}

// Name identifies the backend. It matches config.EngineMLX.
func (m *MLX) Name() string { return config.EngineMLX }

// engine is the string recorded in Result.Engine: "mlx:" plus the model's
// basename, so "mlx-community/Qwen2.5-3B-Instruct-4bit" is reported as
// "mlx:Qwen2.5-3B-Instruct-4bit".
func (m *MLX) engine() string {
	return config.EngineMLX + ":" + modelBasename(m.Model)
}

// Available reports whether the MLX helper is installed and its weights are
// present, by running the helper's --probe. A decoded answer is cached for the
// process lifetime; a timeout is not, because a first probe pays the cost of a
// cold Python start.
func (m *MLX) Available() bool {
	ok, _ := m.probeCache.result(m.probe)
	return ok
}

// detail explains the backend's state for `kagaz doctor`, reusing the cached
// probe rather than running a second one.
func (m *MLX) detail() string {
	ok, why := m.probeCache.result(m.probe)
	if ok {
		path, _ := m.helperPath()
		return path + " (" + m.Model + ")"
	}
	return why
}

// hint names the fix for a forced-but-unavailable mlx engine.
func (m *MLX) hint() string {
	return "run `kagaz model pull --engine mlx` to install " + MLXHelperBinary + " and the model weights"
}

func (m *MLX) helperPath() (string, bool) {
	if m.locate != nil {
		return m.locate()
	}
	return MLXHelperPath()
}

func (m *MLX) runner() helperRunner {
	if m.run != nil {
		return m.run
	}
	return execHelper
}

// probe runs `kagaz-machelper-mlx classify --backend mlx --model <repo> --probe --json`.
func (m *MLX) probe() (bool, string, bool) {
	path, ok := m.helperPath()
	if !ok {
		return false, MLXHelperBinary + " not found (set $" + MLXHelperPathEnv + " for a local build)", false
	}
	if m.Model == "" {
		// A config fact, not a runtime one: safe to cache.
		return false, "no model configured (classify.model)", true
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	out, err := m.runner()(ctx, path, []string{"classify", "--backend", config.EngineMLX, "--model", m.Model, "--probe", "--json"}, "")
	if err != nil {
		return false, err.Error(), !isTimeout(err)
	}
	available, reason := decodeProbeResponse(out)
	return available, reason, true
}

// Classify pipes the document text to kagaz-machelper-mlx on stdin and decodes
// the versioned contract.
func (m *MLX) Classify(ctx context.Context, req Request) (Result, error) {
	path, ok := m.helperPath()
	if !ok {
		return Result{}, fmt.Errorf("%s: %w", MLXHelperBinary, ocr.ErrNoHelper)
	}
	if m.Model == "" {
		return Result{}, fmt.Errorf("%s: no model configured (classify.model)", MLXHelperBinary)
	}

	timeout := m.Timeout
	if timeout <= 0 {
		timeout = mlxClassifyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"classify", "--backend", config.EngineMLX, "--model", m.Model}
	if spec := req.spec(); spec != "" {
		args = append(args, "--doctypes", spec)
	}
	args = append(args, "--json")

	out, err := m.runner()(ctx, path, args, req.text())
	if err != nil {
		return Result{}, err
	}
	return decodeClassifyResponse(MLXHelperBinary, m.engine(), out)
}
