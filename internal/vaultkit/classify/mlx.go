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
// The weights are config.DefaultMLXModel and nothing else -- there is no field
// and no config key to change them. That repo is what `kagaz model pull`
// fetches, what models.PinnedRevision has a build pin for, and what the
// bundled Metal shader library was compiled against, so a second repo would be
// a setting whose other values are all untested. classify.model configures the
// Ollama tier only; it used to feed both, which handed Ollama a Hugging Face
// path it had never heard of.
//
// MLX must not be copied after first use: it caches its probe behind a mutex.
// Hold it by pointer, as Chain does.
type MLX struct {
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

// classifyBudget resolves Timeout against the default, for the same reason
// Apple's does: doctor and Classify must not be able to disagree.
func (m *MLX) classifyBudget() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return mlxClassifyTimeout
}

// timeouts reports this tier's deadlines. The probe budget is the shared one
// -- both helpers pay the same cold-start cost -- while the classification
// budget is four times Apple's, because these weights are loaded from disk.
func (m *MLX) timeouts() *Timeouts {
	return &Timeouts{
		ProbeTimeoutMS:    millis(probeTimeout),
		ClassifyTimeoutMS: millis(m.classifyBudget()),
	}
}

// engine is the string recorded in Result.Engine: "mlx:" plus the model's
// basename, so "mlx-community/Qwen2.5-3B-Instruct-4bit" is reported as
// "mlx:Qwen2.5-3B-Instruct-4bit".
func (m *MLX) engine() string {
	return config.EngineMLX + ":" + modelBasename(config.DefaultMLXModel)
}

// Available reports whether the MLX helper is installed and its weights are
// present, by running the helper's --probe. A decoded answer is cached for the
// process lifetime; a timeout is not, because a first probe pays the cost of a
// cold Python start.
func (m *MLX) Available() bool {
	ok, _, _ := m.probeCache.result(m.probe)
	return ok
}

// detail explains the backend's state for `kagaz doctor`, reusing the cached
// probe rather than running a second one.
func (m *MLX) detail() string {
	ok, why, _ := m.probeCache.result(m.probe)
	if ok {
		path, _ := m.helperPath()
		return path + " (" + config.DefaultMLXModel + ")"
	}
	return why
}

// reason names WHICH of MLX's three independent preconditions is unmet -- the
// helper binary, its Metal shader library, or the weights -- from the stable
// vocabulary in helper.go. Only ReasonWeightsMissing is fixed by
// `kagaz model pull`, and a client offering that download for either of the
// others would cost the user 1.6 GB and change nothing.
func (m *MLX) reason() string {
	ok, _, code := m.probeCache.result(m.probe)
	if ok {
		return ""
	}
	return code
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
func (m *MLX) probe() (bool, string, string, bool) {
	path, ok := m.helperPath()
	if !ok {
		return false,
			MLXHelperBinary + " not found (set $" + MLXHelperPathEnv + " for a local build)",
			ReasonHelperMissing, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	out, err := m.runner()(ctx, path, []string{"classify", "--backend", config.EngineMLX, "--model", config.DefaultMLXModel, "--probe", "--json"}, "")
	if err != nil {
		code := ReasonUnknown
		if isTimeout(err) {
			code = ReasonProbeTimeout
		}
		return false, err.Error(), code, !isTimeout(err)
	}
	available, reason, code := decodeProbeResponse(out)
	return available, reason, code, true
}

// Classify pipes the document text to kagaz-machelper-mlx on stdin and decodes
// the versioned contract.
func (m *MLX) Classify(ctx context.Context, req Request) (Result, error) {
	path, ok := m.helperPath()
	if !ok {
		return Result{}, fmt.Errorf("%s: %w", MLXHelperBinary, ocr.ErrNoHelper)
	}

	ctx, cancel := context.WithTimeout(ctx, m.classifyBudget())
	defer cancel()

	args := []string{"classify", "--backend", config.EngineMLX, "--model", config.DefaultMLXModel}
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
