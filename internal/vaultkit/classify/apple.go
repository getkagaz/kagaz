package classify

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
)

// probeTimeout bounds the availability check. `kagaz doctor` and the auto
// selection path must stay fast when the helper is missing or the model is not
// downloaded.
const probeTimeout = 3 * time.Second

// classifyTimeout bounds one classification. An on-device model that has not
// answered in this long is a hang, and a hang must degrade to rules rather than
// stall ingest.
const classifyTimeout = 30 * time.Second

// Apple runs Apple's Foundation Models through kagaz-machelper. It needs no
// downloaded weights, is the fastest semantic tier, and requires macOS 26 --
// which is exactly why Available() asks the helper rather than assuming.
type Apple struct {
	// Timeout bounds one classification; zero means classifyTimeout.
	Timeout time.Duration

	// run is a test seam. Nil means execHelper.
	run helperRunner
	// locate is a test seam over helper discovery. Nil means ocr.HelperPath.
	locate func() (string, bool)

	// probeOnce caches the probe for the process lifetime: it costs a process
	// spawn, and the answer cannot change while kagaz runs. sync.Once makes
	// concurrent ingest workers safe.
	probeOnce sync.Once
	probeOK   bool
	probeWhy  string
}

// Name identifies the backend. It matches config.EngineApple.
func (a *Apple) Name() string { return config.EngineApple }

// Available reports whether kagaz-machelper is installed and its Apple
// Foundation Models backend is usable, by running the helper's fast --probe.
// The answer is cached for the process lifetime and is race-safe.
func (a *Apple) Available() bool {
	a.probeOnce.Do(a.probe)
	return a.probeOK
}

// detail explains the backend's state for `kagaz doctor`.
func (a *Apple) detail() string {
	if a.Available() {
		path, _ := a.helperPath()
		return path
	}
	return a.probeWhy
}

// hint names the fix for a forced-but-unavailable apple engine.
func (a *Apple) hint() string {
	return "install the macOS helper (`brew install getkagaz/tap/kagaz-machelper`) and run `kagaz doctor`; Apple Foundation Models also require macOS 26"
}

// helperPath resolves kagaz-machelper, reusing ocr's discovery.
func (a *Apple) helperPath() (string, bool) {
	if a.locate != nil {
		return a.locate()
	}
	return helperPath()
}

// probe runs `kagaz-machelper classify --backend apple --probe --json` once.
func (a *Apple) probe() {
	path, ok := a.helperPath()
	if !ok {
		a.probeWhy = ocr.HelperBinary + " not found (macOS only; set $" + ocr.HelperPathEnv + " for a local build)"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	out, err := a.runner()(ctx, path, []string{"classify", "--backend", config.EngineApple, "--probe", "--json"}, "")
	if err != nil {
		a.probeWhy = err.Error()
		return
	}
	available, reason := decodeProbeResponse(out)
	a.probeOK = available
	if !available {
		a.probeWhy = reason
	}
}

// runner returns the injected runner or the real one.
func (a *Apple) runner() helperRunner {
	if a.run != nil {
		return a.run
	}
	return execHelper
}

// Classify pipes the document text to kagaz-machelper on stdin and decodes the
// versioned contract. The returned doctype is unvalidated; the Chain checks it
// against the catalog.
func (a *Apple) Classify(ctx context.Context, req Request) (Result, error) {
	path, ok := a.helperPath()
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrNoHelper, ocr.HelperBinary)
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = classifyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"classify", "--backend", config.EngineApple}
	if spec := req.spec(); spec != "" {
		args = append(args, "--doctypes", spec)
	}
	args = append(args, "--json")

	out, err := a.runner()(ctx, path, args, req.text())
	if err != nil {
		return Result{}, err
	}
	return decodeClassifyResponse(ocr.HelperBinary, config.EngineApple, out)
}
