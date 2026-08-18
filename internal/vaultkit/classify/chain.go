package classify

import (
	"context"
	"fmt"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
)

// Chain implements the classify.engine setting over the tiered backends.
//
// # The tier order
//
//	auto    apple -> mlx (if available) -> ollama (if available) -> rules
//	apple   apple -> rules
//	mlx     mlx   -> rules
//	ollama  ollama -> rules
//	rules   rules only; no model is ever run and no probe is ever taken
//
// auto chains every semantic tier the machine actually has, because a tier
// that declines or misfires does not bind the next one -- that is the point of
// having several. `rules` is the explicit "no LLM used" choice.
//
// # The fallback matrix
//
// "next" means the next tier in the order above, and rules when there is none.
//
//	tier unavailable (no binary/weights/daemon) -> next, skipped on a cached probe
//	tier declines (doctypes.Unclassified)       -> next
//	tier doctype not in the catalog             -> next
//	tier confidence < min_confidence            -> next
//	tier exits non-zero (structured err)        -> next
//	tier speaks an unknown contract             -> next
//	tier emits malformed JSON                   -> next
//	tier times out                              -> next
//	tier category disagrees w/ the catalog      -> catalog's category wins
//	caller's context cancelled mid-chain        -> stop, rules (in-process)
//	rules unconfident or unmatched              -> doctypes.Unclassified, 0.0
//	forced engine unavailable                   -> error naming the fix
//	forced engine available but failing         -> rules
//
// Only the forced-unavailable row is an error. Everything else degrades,
// because a classifier problem must never fail an ingest.
//
// # Bounding the chain
//
// Each tier is attempted at most once per document: the order is a slice of
// distinct backends and the loop never revisits one. Availability is decided
// by each backend's own memoised probe (probeCache for the two Swift helpers,
// a TTL for Ollama), so an absent tier costs a map lookup rather than a helper
// launch per document. The caller's context is checked before each tier, so a
// cancelled ingest stops the chain instead of paying out the remaining tiers.
//
// Worst case under auto is therefore one classifyTimeout (30s, apple) plus one
// mlxClassifyTimeout (2m) plus one ollamaClassifyTimeout (2m) = 4m30s for a
// single document on a machine where all three tiers are installed and all
// three hang -- unchanged per tier, and reached only by a document that every
// installed model both answers slowly and fails on. In the ordinary declining
// case the cost is three real inferences instead of one.
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
		Engine:        config.EngineAuto,
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
		c.MLX = &MLX{Model: cfg.Classify.Model}
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

	tiers, err := c.semanticTiers()
	if err != nil {
		// The one hard failure: the user forced an engine that is not
		// installed, and silently using a different one would misreport
		// provenance in every sidecar written afterwards.
		return Result{}, err
	}

	for _, tier := range tiers {
		// Cancellation stops the chain rather than paying out the remaining
		// tiers. Rules still runs: it is pure in-process regex work that
		// ignores the context entirely, so it costs nothing to answer with,
		// and the alternative -- a hard error -- would fail an ingest for a
		// classifier reason, which invariant 4 forbids.
		if ctx.Err() != nil {
			break
		}
		if res, ok := c.trySemantic(ctx, tier, req, cat); ok {
			return res, nil
		}
	}
	return c.rulesResult(ctx, req, cat), nil
}

// trySemantic runs a model backend and validates its answer. The bool reports
// whether the answer was accepted; false means "try the next tier", and the
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
		// it would otherwise look like. The next tier still gets its turn:
		// one model declining does not bind another, and a keyword or a
		// machine-readable zone can recognise a document no model could. An
		// unmatched rules tier lands on Unclassified anyway.
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
	// caller of the chain, would otherwise have to remember to repeat it.
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

// semanticTiers returns the model tiers to attempt, in order, for the
// configured engine. An empty slice with a nil error means "go straight to
// rules"; a non-nil error means the user forced an engine that is not
// installed.
//
// Every element is a distinct backend, which is what makes "each tier at most
// once per document" a property of the data rather than of the loop. Under
// auto the list is filtered by Available(), so an unavailable tier is never
// entered at all; that call is each backend's memoised probe, not a fresh
// helper launch.
func (c *Chain) semanticTiers() ([]Classifier, error) {
	switch c.Engine {
	case "", config.EngineAuto:
		// auto chains every tier this machine actually has, cheapest first:
		// apple needs no weights, mlx has them on disk, ollama is a daemon.
		// A tier that is not installed is skipped rather than paid for, so the
		// cost of the longer chain lands only on machines that opted into the
		// extra tiers by installing them.
		var tiers []Classifier
		if c.Apple != nil && c.Apple.Available() {
			tiers = append(tiers, c.Apple)
		}
		if c.MLX != nil && c.MLX.Available() {
			tiers = append(tiers, c.MLX)
		}
		if c.Ollama != nil && c.Ollama.Available() {
			tiers = append(tiers, c.Ollama)
		}
		return tiers, nil

	case config.EngineRules:
		// The explicit "no LLM used" choice. Nothing below this line may probe
		// or spawn a helper: not even Available() is called, so `rules` costs
		// no process and no socket.
		return nil, nil

	case config.EngineApple:
		return forcedTier(c.Apple, config.EngineApple)
	case config.EngineMLX:
		return forcedTier(c.MLX, config.EngineMLX)
	case config.EngineOllama:
		return forcedTier(c.Ollama, config.EngineOllama)

	default:
		return nil, fmt.Errorf("classify.engine %q is not one of %s, %s, %s, %s, %s",
			c.Engine, config.EngineAuto, config.EngineApple, config.EngineMLX, config.EngineOllama, config.EngineRules)
	}
}

// forcedTier wraps forced as a one-element tier list.
func forcedTier[T Classifier](backend T, engine string) ([]Classifier, error) {
	b, err := forced(backend, engine)
	if err != nil {
		return nil, err
	}
	return []Classifier{b}, nil
}

// Plan is what Classify will actually do on this machine right now, for
// `kagaz doctor`.
type Plan struct {
	// Engine is the configured classify.engine.
	Engine string `json:"engine"`
	// Order lists the tiers that will be attempted, in order, always ending
	// in "rules". Empty only when Err is set.
	Order []string `json:"order,omitempty"`
	// Skipped lists tiers the configured engine would have used but which are
	// unavailable right now.
	Skipped []string `json:"skipped,omitempty"`
	// Err is set when the configured engine is forced and unavailable, which
	// is the one condition that fails a classification outright.
	Err string `json:"error,omitempty"`
}

// Plan reports the tier order Classify will follow, so doctor and the pipeline
// agree by construction. It reuses each backend's cached probe.
func (c *Chain) Plan() Plan {
	p := Plan{Engine: c.Engine}
	if p.Engine == "" {
		p.Engine = config.EngineAuto
	}
	tiers, err := c.semanticTiers()
	if err != nil {
		p.Err = err.Error()
		return p
	}
	for _, t := range tiers {
		p.Order = append(p.Order, t.Name())
	}
	if p.Engine == config.EngineAuto {
		for _, s := range c.Describe() {
			if s.Name != config.EngineRules && !s.Available {
				p.Skipped = append(p.Skipped, s.Name)
			}
		}
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
		out = append(out, Status{Name: config.EngineApple, Available: c.Apple.Available(), Detail: c.Apple.detail()})
	}
	if c.MLX != nil {
		out = append(out, Status{Name: config.EngineMLX, Available: c.MLX.Available(), Detail: c.MLX.detail()})
	}
	if c.Ollama != nil {
		out = append(out, Status{Name: config.EngineOllama, Available: c.Ollama.Available(), Detail: c.Ollama.detail()})
	}
	return out
}

// Status is one backend's availability, for doctor output.
type Status struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}
