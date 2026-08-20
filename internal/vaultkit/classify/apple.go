package classify

import (
	"context"
	"fmt"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
)

// classifyTimeout bounds one classification, after which the tier degrades to
// rules rather than stalling ingest.
//
// It was 30s, on the premise that an on-device model quiet for that long has
// hung. Measurement says otherwise: over 11 consecutive classifications of one
// short statement against a 977-character doctype spec, Apple's Foundation
// Models returned in 4.5s at the median and 4.5-4.7s once warm, but with a long
// tail -- 13s, 14s, 19s, 22s, and two answers at 51s and 60s. Two of eleven
// exceeded the old budget, and the 60s one arrived after several warm calls, so
// this is not a cold-start effect that goes away.
//
// Those slow calls returned correct answers. Killing them reported a document
// as unclassifiable when the tier simply had not finished, which is what made a
// first-run SKIP look like a fact about the file.
//
// 90s clears the slowest observed answer with room, and stays strictly under
// the two-minute budgets mlx and ollama use -- an ordering doctor's own test
// asserts, because the tier needing no download should be the one that gives up
// first.
const classifyTimeout = 90 * time.Second

// Apple runs Apple's Foundation Models through kagaz-machelper. It needs no
// downloaded weights, is the fastest semantic tier, and requires macOS 26 --
// which is exactly why Available() asks the helper rather than assuming.
//
// Apple must not be copied after first use: it caches its probe behind a
// mutex. Hold it by pointer, as Chain does.
type Apple struct {
	// Timeout bounds one classification; zero means classifyTimeout.
	Timeout time.Duration

	// run is a test seam. Nil means execHelper.
	run helperRunner
	// locate is a test seam over helper discovery. Nil means ocr.HelperPath.
	locate func() (string, bool)

	probeCache probeCache
}

// Name identifies the backend. It matches config.EngineApple.
func (a *Apple) Name() string { return config.EngineApple }

// classifyBudget resolves Timeout against the default. Classify and timeouts()
// both read it, so what doctor reports and what Classify enforces cannot drift.
func (a *Apple) classifyBudget() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return classifyTimeout
}

// timeouts reports this tier's deadlines. Apple's classification budget is
// seconds where the two loading tiers get minutes, which is precisely why the
// numbers are per tier and why a client stating one global figure is wrong
// about the default engine.
func (a *Apple) timeouts() *Timeouts {
	return &Timeouts{
		ProbeTimeoutMS:    millis(probeTimeout),
		ClassifyTimeoutMS: millis(a.classifyBudget()),
	}
}

// Available reports whether kagaz-machelper is installed and its Apple
// Foundation Models backend is usable, by running the helper's fast --probe.
// A decoded answer is cached for the process lifetime; a timeout is not. It is
// race-safe.
func (a *Apple) Available() bool {
	ok, _, _ := a.probeCache.result(a.probe)
	return ok
}

// detail explains the backend's state for `kagaz doctor`. It reuses the cached
// probe rather than running a second one.
func (a *Apple) detail() string {
	ok, why, _ := a.probeCache.result(a.probe)
	if ok {
		path, _ := a.helperPath()
		return path
	}
	return why
}

// reason names WHICH precondition is unmet, from the stable vocabulary in
// helper.go, for a client that must decide what to offer the user. Empty when
// the tier is available.
func (a *Apple) reason() string {
	ok, _, code := a.probeCache.result(a.probe)
	if ok {
		return ""
	}
	return code
}

// hint names the fix for a forced-but-unavailable apple engine.
func (a *Apple) hint() string {
	return "build the macOS helper (`swift build --package-path machelper -c release`), put kagaz-machelper on your PATH, and run `kagaz doctor`; Apple Foundation Models also require macOS 26"
}

// helperPath resolves kagaz-machelper, reusing ocr's discovery.
func (a *Apple) helperPath() (string, bool) {
	if a.locate != nil {
		return a.locate()
	}
	return helperPath()
}

// probe runs `kagaz-machelper classify --backend apple --probe --json` once.
func (a *Apple) probe() (bool, string, string, bool) {
	path, ok := a.helperPath()
	if !ok {
		// Not cached: the user may install the helper while kagaz runs, and
		// re-checking costs one stat.
		return false,
			ocr.HelperBinary + " not found (macOS only; set $" + ocr.HelperPathEnv + " for a local build)",
			ReasonHelperMissing, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	out, err := a.runner()(ctx, path, []string{"classify", "--backend", config.EngineApple, "--probe", "--json"}, "")
	if err != nil {
		// A timeout is a cold start until proven otherwise: never cached.
		code := ReasonUnknown
		if isTimeout(err) {
			code = ReasonProbeTimeout
		}
		return false, err.Error(), code, !isTimeout(err)
	}
	available, reason, code := decodeProbeResponse(out)
	return available, reason, code, true
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
		return Result{}, fmt.Errorf("%s: %w", ocr.HelperBinary, ocr.ErrNoHelper)
	}

	ctx, cancel := context.WithTimeout(ctx, a.classifyBudget())
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
