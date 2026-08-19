package classify

import (
	"context"
	"fmt"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
)

// Chain implements the classify.engine setting over the tiered backends.
//
// # The four engines
//
//	apple   apple  -> rules   (the default)
//	mlx     mlx    -> rules
//	ollama  ollama -> rules
//	rules   rules only; no model is ever run and no probe is ever taken
//
// Falling back to the deterministic tier is part of each engine's definition
// rather than a setting of its own: every model engine ends at rules, and
// `rules` is the explicit "no LLM used" choice. There is no "mlx or nothing"
// mode and no `auto` -- config rejects that value rather than guessing what
// the user meant by it.
//
// apple is the default because it needs nothing downloaded and nothing
// running: on a Mac that has Apple's on-device model it reads the document,
// and on one that does not the rules tier answers.
//
// # The fallback matrix
//
//	engine=apple, apple available            -> apple, validated
//	engine=apple, apple absent               -> rules
//	semantic declines (doctypes.Unclassified) -> rules
//	semantic doctype not in the catalog      -> rules
//	semantic confidence < min_confidence     -> rules
//	semantic category disagrees w/ catalog   -> catalog's category wins
//	helper exits non-zero (structured err)   -> rules
//	helper speaks an unknown contract        -> rules
//	helper emits malformed JSON              -> rules
//	helper times out                         -> rules
//	rules unconfident or unmatched           -> doctypes.Unclassified, 0.0
//	engine=mlx/ollama unavailable            -> error naming the fix
//	engine=mlx/ollama available but failing  -> rules
//
// Only the unavailable-engine row is an error, and only for the two engines a
// user installs on purpose: asking for MLX and silently getting keyword
// matching would misreport provenance in every sidecar written afterwards,
// whereas apple is the default and must work on a Mac that never had the
// model. Everything else degrades, because a classifier problem must never
// fail an ingest.
//
// Result.Engine names the tier that produced the accepted answer, so the
// sidecar's provenance is never a guess.
type Chain struct {
	// Engine is one of the config.Engine* constants. Empty means auto.
	Engine string
	// MinConfidence is the acceptance threshold for a semantic answer and for
	// a rules answer alike. Zero means every non-Unclassified answer is kept.
	MinConfidence float64
	// Catalog is the resolved catalog used when a Request carries none.
	Catalog *doctypes.Catalog

	// Backends. Each may be nil, in which case it is simply unavailable.
	Rules  *Rules
	Apple  *Apple
	MLX    *MLX
	Ollama *Ollama
}

// New builds the chain for a vault from its config and resolved catalog.
func New(cfg *config.Config, cat *doctypes.Catalog) *Chain {
	c := &Chain{
		Engine:        config.EngineDefault,
		MinConfidence: 0.5,
		Catalog:       cat,
		Rules:         &Rules{Catalog: cat},
		Apple:         &Apple{},
	}
	if cfg != nil {
		if cfg.Classify.Engine != "" {
			c.Engine = cfg.Classify.Engine
		}
		if cfg.Classify.MinConfidence > 0 {
			c.MinConfidence = cfg.Classify.MinConfidence
		}
		// classify.model and classify.endpoint are Ollama's alone. MLX is
		// pinned to config.DefaultMLXModel (see the MLX type): one key cannot
		// name a model for two engines whose naming schemes share nothing, and
		// the version that tried handed Ollama a Hugging Face repo path.
		c.MLX = &MLX{}
		c.Ollama = &Ollama{Endpoint: cfg.Classify.Endpoint, Model: cfg.Classify.Model}
	}
	return c
}

// Name identifies the chain itself in doctor output.
func (c *Chain) Name() string { return "chain" }

// Available is always true: the rules tier is always usable.
func (c *Chain) Available() bool { return true }

// Classify runs the configured tier, validates its answer against the catalog,
// and degrades as documented on Chain. The only error it returns is a forced
// engine that is not installed.
func (c *Chain) Classify(ctx context.Context, req Request) (Result, error) {
	cat := req.Catalog
	if cat == nil {
		cat = c.Catalog
	}
	if cat == nil {
		return Result{}, fmt.Errorf("classify: no doctype catalog resolved")
	}
	req.Catalog = cat
	if req.DocTypes == "" {
		req.DocTypes = cat.Spec()
	}

	semantic, err := c.semanticBackend()
	if err != nil {
		// The one hard failure: the user named an engine that is not
		// installed, and silently using a different one would misreport
		// provenance in every sidecar written afterwards.
		return Result{}, err
	}

	if semantic != nil {
		if res, ok := c.trySemantic(ctx, semantic, req, cat); ok {
			return res, nil
		}
	}
	return c.rulesResult(ctx, req, cat), nil
}

// trySemantic runs a model backend and validates its answer. The bool reports
// whether the answer was accepted; false means "fall back to rules", and the
// reason is deliberately not surfaced as an error, because none of these
// conditions should fail an ingest.
func (c *Chain) trySemantic(ctx context.Context, backend Classifier, req Request, cat *doctypes.Catalog) (Result, bool) {
	raw, err := backend.Classify(ctx, req)
	if err != nil {
		// Structured helper error, unknown contract, malformed JSON, timeout,
		// non-zero exit: all the same from here.
		return Result{}, false
	}
	if declined(raw) {
		// The model used its escape hatch: it was offered doctypes.Unclassified
		// alongside the catalog and answered that none of them fits. That is a
		// deliberate, useful answer, not the unknown-doctype validation failure
		// it would otherwise look like. Rules still get their turn -- a keyword
		// or a machine-readable zone can recognise a document the model could
		// not -- and an unmatched rules tier lands on Unclassified anyway.
		return Result{}, false
	}
	res, ok := validate(cat, raw, c.MinConfidence)
	if !ok {
		// Unknown doctype, or below min_confidence.
		return Result{}, false
	}
	// Deterministic extraction stays authoritative for structured fields; the
	// model's fields only fill gaps the catalog has no template for, and only
	// when the value they carry is actually in the document (see grounder).
	// This is the only place every backend's fields pass through, which is why
	// the check lives here and not in ingest: a second backend, or a second
	// caller of the chain, would otherwise have to remember to repeat it. It
	// is unconditional -- no setting can switch the catalog's regexes off and
	// quietly empty invoice_number, amount and every date out of a sidecar.
	res.Fields, res.Dropped = mergeFields(cat.ExtractFields(res.DocType, req.Text), raw.Fields, req.Text)
	return res, true
}

// rulesResult runs the deterministic tier and applies the same validation. An
// unmatched or unconfident rules answer becomes Unclassified with zero
// confidence rather than a guess.
func (c *Chain) rulesResult(ctx context.Context, req Request, cat *doctypes.Catalog) Result {
	rules := c.Rules
	if rules == nil {
		rules = &Rules{Catalog: cat}
	}
	raw, err := rules.Classify(ctx, req)
	if err != nil {
		return unclassified()
	}
	res, ok := validate(cat, raw, c.MinConfidence)
	if !ok {
		return unclassified()
	}
	// The rules tier's own fields are regex captures over this same text, so
	// they arrive on the deterministic side and are never grounding-checked:
	// they cannot be anything but grounded.
	res.Fields, res.Dropped = mergeFields(cat.ExtractFields(res.DocType, req.Text), raw.Fields, req.Text)
	return res
}

// semanticBackend picks the model tier for the configured engine. A nil
// backend with a nil error means "go straight to rules"; a non-nil error means
// the user named an engine that is not installed.
func (c *Chain) semanticBackend() (Classifier, error) {
	switch c.Engine {
	case "", config.EngineApple:
		// The default, and the one engine whose tier may simply be absent:
		// Apple's on-device model does not exist before macOS 26, and a
		// default that errored there would make Kagaz unusable out of the box.
		// Availability comes from the backend's memoised probe, so an absent
		// tier costs a lookup rather than a helper launch per document.
		if c.Apple != nil && c.Apple.Available() {
			return c.Apple, nil
		}
		return nil, nil

	case config.EngineRules:
		// The explicit "no LLM used" choice. Nothing below this line may probe
		// or spawn a helper: not even Available() is called, so `rules` costs
		// no process and no socket.
		return nil, nil

	case config.EngineMLX:
		// Installed on purpose, so absence is an error naming the fix rather
		// than a quiet downgrade to keyword matching.
		return forced(c.MLX, config.EngineMLX)
	case config.EngineOllama:
		return forced(c.Ollama, config.EngineOllama)

	default:
		return nil, fmt.Errorf("classify.engine %q is not one of %s, %s, %s, %s",
			c.Engine, config.EngineApple, config.EngineMLX, config.EngineOllama, config.EngineRules)
	}
}

// Plan is what Classify will actually do on this machine right now, for
// `kagaz doctor`.
type Plan struct {
	// Engine is the configured classify.engine.
	Engine string `json:"engine"`
	// Order lists the tiers that will be attempted, in order, always ending
	// in "rules". Empty only when Err is set.
	Order []string `json:"order,omitempty"`
	// Skipped lists tiers this engine would have used but which are
	// unavailable right now. Only apple can appear: the other two engines are
	// an error when unavailable rather than a skip.
	Skipped []string `json:"skipped,omitempty"`
	// Err is set when the configured engine is one of the installed-on-purpose
	// tiers and it is unavailable, which is the one condition that fails a
	// classification outright.
	Err string `json:"error,omitempty"`
}

// Plan reports the tier order Classify will follow, so doctor and the pipeline
// agree by construction. It reuses each backend's cached probe.
func (c *Chain) Plan() Plan {
	p := Plan{Engine: c.Engine}
	if p.Engine == "" {
		p.Engine = config.EngineDefault
	}
	semantic, err := c.semanticBackend()
	if err != nil {
		p.Err = err.Error()
		return p
	}
	if semantic != nil {
		p.Order = append(p.Order, p.Engine)
	} else if p.Engine != config.EngineRules {
		p.Skipped = append(p.Skipped, p.Engine)
	}
	p.Order = append(p.Order, config.EngineRules)
	return p
}

// forced returns a explicitly-selected backend, or an error naming the fix when
// it is unavailable. The nil check is on the interface's concrete value, so a
// Chain built without a backend reports the same actionable message.
func forced[T Classifier](backend T, engine string) (Classifier, error) {
	if isNilBackend(backend) {
		return nil, fmt.Errorf("classify.engine %q is not available on this machine", engine)
	}
	if !backend.Available() {
		msg := fmt.Sprintf("classify.engine %q is not available on this machine", engine)
		if h, ok := any(backend).(hinter); ok {
			msg += ": " + h.hint()
		}
		if d, ok := any(backend).(detailer); ok {
			if detail := d.detail(); detail != "" {
				msg += " (" + detail + ")"
			}
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return backend, nil
}

// detailer is implemented by backends that can explain their current state.
type detailer interface {
	detail() string
}

// isNilBackend reports whether a typed-nil pointer was passed as a backend.
func isNilBackend[T Classifier](backend T) bool {
	switch v := any(backend).(type) {
	case *Apple:
		return v == nil
	case *MLX:
		return v == nil
	case *Ollama:
		return v == nil
	case *Rules:
		return v == nil
	default:
		return false
	}
}

// Describe reports each backend's availability, for `kagaz doctor`.
func (c *Chain) Describe() []Status {
	out := []Status{{Name: config.EngineRules, Available: true, Detail: "built in"}}
	if c.Apple != nil {
		out = append(out, Status{Name: config.EngineApple, Available: c.Apple.Available(),
			Detail: c.Apple.detail(), Reason: c.Apple.reason(), Timeouts: c.Apple.timeouts()})
	}
	if c.MLX != nil {
		// The pinned repo regardless of Available: a client asking "what would
		// MLX load here" is usually asking precisely because it cannot load.
		out = append(out, Status{Name: config.EngineMLX, Available: c.MLX.Available(),
			Detail: c.MLX.detail(), Reason: c.MLX.reason(), Model: config.DefaultMLXModel,
			Timeouts: c.MLX.timeouts()})
	}
	if c.Ollama != nil {
		out = append(out, Status{Name: config.EngineOllama, Available: c.Ollama.Available(),
			Detail: c.Ollama.detail(), Reason: c.Ollama.reason(), Model: c.Ollama.Model,
			Timeouts: c.Ollama.timeouts()})
	}
	return out
}

// Status is one backend's availability, for doctor output.
type Status struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	// Detail is prose for a human and is reworded whenever it reads better.
	Detail string `json:"detail,omitempty"`
	// Reason is WHICH precondition is unmet, from the stable vocabulary in
	// helper.go, and is empty when the tier is available. It exists so a
	// client never has to pattern-match Detail: "the weights are missing" and
	// "the helper is missing" look alike in prose and need opposite actions.
	Reason string `json:"reason,omitempty"`
	// Model is the identifier of the weights this tier would load, and exists
	// for the same reason Reason does: so a client never has to derive it. The
	// alternative is a client hardcoding a copy of config.DefaultMLXModel,
	// which goes stale silently the day the pin moves. It is set whether or
	// not the tier is available -- "what would MLX load" is a fair question on
	// a machine where MLX cannot run -- and empty for the tiers that load no
	// weights at all, and for an Ollama with no classify.model, where naming a
	// default the user never chose would be the same invention.
	Model string `json:"model,omitempty"`
	// Timeouts are the deadlines this tier enforces, and exist for the same
	// reason Reason and Model do: a client must never transcribe a value the
	// CLI owns. They are per tier and they differ -- apple bounds one
	// classification at thirty seconds where mlx and ollama get two minutes --
	// so a UI stating one global figure is wrong about the default engine, and
	// wrong in the direction that makes it look hung. Nil for a tier that runs
	// no model and therefore has no deadline to report; the key is then absent
	// rather than an empty object, so "has timeouts" and "has a probe budget"
	// cannot disagree.
	Timeouts *Timeouts `json:"timeouts,omitempty"`
}

// Timeouts is one tier's deadlines, in whole milliseconds.
//
// Milliseconds and not a duration string: the rule Reason states -- a client
// must never pattern-match a human sentence -- applies just as much to "1.5s",
// which is prose about a number. An integer is the one shape a client cannot
// misread, and formatting it for a person is the client's job.
//
// Each name says what it bounds, because a probe and a classification are
// different things by two orders of magnitude on the ollama tier and a reader
// of the JSON must not have to guess which one a bare "timeout" meant.
type Timeouts struct {
	// ProbeTimeoutMS bounds one availability probe -- the call Available() and
	// doctor make -- not the time a probe took.
	ProbeTimeoutMS int64 `json:"probe_timeout_ms,omitempty"`
	// ProbeCacheTTLMS is how long one probe answer is reused before the tier is
	// asked again. It is not a deadline on anything: it is why a tier can go on
	// reporting "unreachable" for a moment after the daemon starts. Only the
	// ollama tier caches on a clock; the helper tiers cache a decisive answer
	// for the process lifetime and so report none.
	ProbeCacheTTLMS int64 `json:"probe_cache_ttl_ms,omitempty"`
	// ClassifyTimeoutMS bounds one document's classification, after which the
	// tier degrades to rules. This is the number a UI needs to decide how long
	// a spinner may honestly keep spinning.
	ClassifyTimeoutMS int64 `json:"classify_timeout_ms,omitempty"`
}

// millis converts a duration for the wire. Every deadline this package holds
// is a whole number of milliseconds, so nothing is lost, and an integer field
// cannot acquire a unit suffix the way a formatted string does.
func millis(d time.Duration) int64 { return int64(d / time.Millisecond) }
