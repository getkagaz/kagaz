package classify

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// mlxClassifyTimeout is more generous than the Apple tier: an MLX model loads
// several gigabytes of weights on first use.
const mlxClassifyTimeout = 2 * time.Minute

// MLX runs a local quantised model through kagaz-machelper-mlx. It speaks the
// same versioned contract as the Apple backend but is a separate binary and
// needs weights on disk, fetched by `kagaz model pull --engine mlx`.
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

	probeOnce sync.Once
	probeOK   bool
	probeWhy  string
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
// present, by running the helper's fast --probe. The answer is cached for the
// process lifetime and is race-safe.
func (m *MLX) Available() bool {
	m.probeOnce.Do(m.probe)
	return m.probeOK
}

// detail explains the backend's state for `kagaz doctor`.
func (m *MLX) detail() string {
	if m.Available() {
		path, _ := m.helperPath()
		return path + " (" + m.Model + ")"
	}
	return m.probeWhy
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
func (m *MLX) probe() {
	path, ok := m.helperPath()
	if !ok {
		m.probeWhy = MLXHelperBinary + " not found (set $" + MLXHelperPathEnv + " for a local build)"
		return
	}
	if m.Model == "" {
		m.probeWhy = "no model configured (classify.model)"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	out, err := m.runner()(ctx, path, []string{"classify", "--backend", config.EngineMLX, "--model", m.Model, "--probe", "--json"}, "")
	if err != nil {
		m.probeWhy = err.Error()
		return
	}
	available, reason := decodeProbeResponse(out)
	m.probeOK = available
	if !available {
		m.probeWhy = reason
	}
}

// Classify pipes the document text to kagaz-machelper-mlx on stdin and decodes
// the versioned contract.
func (m *MLX) Classify(ctx context.Context, req Request) (Result, error) {
	path, ok := m.helperPath()
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrNoHelper, MLXHelperBinary)
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
