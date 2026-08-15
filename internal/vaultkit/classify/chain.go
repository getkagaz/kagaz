package classify

import (
	"context"
	"fmt"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
)

// Chain implements the classify.engine setting over the tiered backends.
//
// The fallback matrix it guarantees:
//
//	engine=auto, apple available            -> apple, validated
//	engine=auto, apple absent               -> rules
//	semantic doctype not in the catalog     -> rules
//	semantic confidence < min_confidence    -> rules
//	semantic category disagrees w/ catalog  -> catalog's category wins
//	helper exits non-zero (structured err)  -> rules
//	helper speaks an unknown contract       -> rules
//	helper emits malformed JSON             -> rules
//	helper times out                        -> rules
//	rules unconfident or unmatched          -> doctypes.Unclassified, 0.0
//	forced engine unavailable               -> error naming the fix
//	forced engine available but failing     -> rules
//
// Only the forced-unavailable row is an error. Everything else degrades,
// because a classifier problem must never fail an ingest.
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

	semantic, err := c.semanticBackend()
	if err != nil {
		// The one hard failure: the user forced an engine that is not
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
	res, ok := validate(cat, raw, c.MinConfidence)
	if !ok {
		// Unknown doctype, or below min_confidence.
		return Result{}, false
	}
	// Deterministic extraction stays authoritative for structured fields; the
	// model's fields only fill gaps the catalog has no template for.
	res.Fields = mergeFields(cat.ExtractFields(res.DocType, req.Text), raw.Fields)
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
	res.Fields = mergeFields(cat.ExtractFields(res.DocType, req.Text), raw.Fields)
	return res
}

// semanticBackend picks the model tier for the configured engine. A nil
// backend with a nil error means "go straight to rules"; a non-nil error means
// the user forced an engine that is not installed.
func (c *Chain) semanticBackend() (Classifier, error) {
	switch c.Engine {
	case "", config.EngineAuto:
		// auto tries apple only. MLX and Ollama are opt-in: they cost weights
		// on disk and minutes of CPU, which is not something to fall into.
		if c.Apple != nil && c.Apple.Available() {
			return c.Apple, nil
		}
		return nil, nil

	case config.EngineRules:
		return nil, nil

	case config.EngineApple:
		return forced(c.Apple, config.EngineApple)
	case config.EngineMLX:
		return forced(c.MLX, config.EngineMLX)
	case config.EngineOllama:
		return forced(c.Ollama, config.EngineOllama)

	default:
		return nil, fmt.Errorf("classify.engine %q is not one of %s, %s, %s, %s, %s",
			c.Engine, config.EngineAuto, config.EngineApple, config.EngineMLX, config.EngineOllama, config.EngineRules)
	}
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
