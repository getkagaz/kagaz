// Package classify decides what kind of document a piece of text is.
//
// Classification is semantic-first: an on-device model (Apple Foundation
// Models through kagaz-machelper, an MLX model, or a local Ollama model) reads
// the text and names a doctype. Deterministic keyword rules
// (doctypes.Catalog.Classify) are the offline fallback and remain the source of
// structured field extraction. This deliberately replaces an earlier
// regex-only design that was an unbounded maintenance treadmill.
//
// Three invariants hold on every path through this package:
//
//  1. No network at classify time. The Ollama backend re-validates that its
//     endpoint is loopback on every call and never trusts config alone.
//  2. Model output is never trusted. A returned doctype must exist in the
//     resolved catalog, and the category always comes from the catalog, never
//     from the model.
//  3. Graceful degradation, never a hard failure. A missing, broken, slow or
//     lying backend degrades to rules, and an unconfident rules answer degrades
//     to doctypes.Unclassified with zero confidence. Ingest is never failed by
//     a classifier problem.
package classify

import (
	"context"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
)

// Contract is the machelper JSON contract version this build understands. The
// helper stamps every response with "contract"; anything else is rejected
// rather than guessed at, so a mismatched helper degrades to rules instead of
// feeding the Go core fields that mean something different.
const Contract = 1

// maxText bounds how much document text is handed to a model backend. A
// doctype is decided by the first page in practice, and an unbounded prompt is
// a reliable way to make a local model slow or to blow its context window.
// Rules always see the full text.
const maxText = 20000

// Request is one classification job.
type Request struct {
	// Text is the extracted document text.
	Text string
	// Path is the document's path, used only for error messages and never
	// sent to a model.
	Path string
	// DocTypes is the compact catalog spec "name:category,..." passed to the
	// helper so the model's output is constrained to real doctypes. Empty
	// means it is derived from Catalog.
	DocTypes string
	// Catalog is the resolved doctype catalog this result is validated
	// against. It overrides the Chain's own catalog when set.
	Catalog *doctypes.Catalog
}

// Result is an accepted classification and its provenance.
type Result struct {
	// DocType is a name that exists in the resolved catalog, or
	// doctypes.Unclassified.
	DocType string
	// Category is the catalog's category for DocType. It is never taken from
	// a model.
	Category string
	// Confidence is 0-1. Zero for Unclassified.
	Confidence float64
	// Fields are extracted structured values; nil when there are none.
	Fields map[string]string
	// Engine records which backend produced the accepted answer:
	// "apple" | "rules" | "mlx:<model basename>" | "ollama:<model>". It lands
	// in the sidecar's `classifier` field.
	Engine string
}

// Classifier is one classification backend.
type Classifier interface {
	// Name is the backend's stable short name ("rules", "apple", "mlx",
	// "ollama"), matching the config.Engine* constants.
	Name() string
	// Available reports whether the backend can run right now. It must be
	// cheap and must never fail an operation.
	Available() bool
	// Classify returns the backend's own opinion. The doctype is unvalidated:
	// the Chain checks it against the catalog.
	Classify(ctx context.Context, req Request) (Result, error)
}

// hinter is implemented by backends that can explain how to install
// themselves. A forced-but-unavailable engine is the one classifier condition
// that is a hard error, so its message must name the fix.
type hinter interface {
	hint() string
}

// spec returns the catalog spec string to constrain a model with.
func (r Request) spec() string {
	if r.DocTypes != "" {
		return r.DocTypes
	}
	if r.Catalog != nil {
		return r.Catalog.Spec()
	}
	return ""
}

// text returns the document text clipped to maxText runes.
func (r Request) text() string {
	if len(r.Text) <= maxText {
		return r.Text
	}
	// Clip on a rune boundary so a model never sees a mangled final character.
	clipped := r.Text[:maxText]
	for len(clipped) > 0 {
		if c, size := utf8.DecodeLastRuneInString(clipped); c == utf8.RuneError && size <= 1 {
			clipped = clipped[:len(clipped)-1]
			continue
		}
		break
	}
	return clipped
}

// unclassified is the zero-confidence answer used whenever nothing is
// trustworthy. Engine is "rules" because the deterministic tier is what
// actually produced it.
func unclassified() Result {
	return Result{DocType: doctypes.Unclassified, Confidence: 0, Engine: config.EngineRules}
}

// validate enforces Global Constraint 8 on a backend's raw answer: the doctype
// must exist in the resolved catalog, the confidence must be a real number at
// or above min, and the category is replaced with the catalog's. A model that
// disagrees with the catalog about the category loses, always.
func validate(cat *doctypes.Catalog, res Result, min float64) (Result, bool) {
	if cat == nil {
		return Result{}, false
	}
	name := config.Slug(strings.TrimSpace(res.DocType))
	if name == "" || name == doctypes.Unclassified {
		return Result{}, false
	}
	dt, ok := cat.Get(name)
	if !ok {
		return Result{}, false
	}
	conf := res.Confidence
	if math.IsNaN(conf) || math.IsInf(conf, 0) {
		return Result{}, false
	}
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	if conf < min {
		return Result{}, false
	}
	return Result{
		DocType:    dt.Name,
		Category:   dt.Category, // catalog wins; never the model's category
		Confidence: conf,
		Fields:     res.Fields,
		Engine:     res.Engine,
	}, true
}

// mergeFields combines deterministic extraction with a model's fields.
// Deterministic values win: a regex that fired on the real document text is
// better evidence than a value a model may have paraphrased. Model fields fill
// the gaps the catalog has no template for.
func mergeFields(deterministic, model map[string]string) map[string]string {
	if len(deterministic) == 0 && len(model) == 0 {
		return nil
	}
	out := make(map[string]string, len(deterministic)+len(model))
	for k, v := range model {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	for k, v := range deterministic {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// modelBasename reduces a Hugging Face repo path to the name a user recognises,
// so "mlx-community/Qwen2.5-3B-Instruct-4bit" becomes "Qwen2.5-3B-Instruct-4bit"
// in the sidecar's classifier field.
func modelBasename(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}
