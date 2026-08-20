// Package classify decides what kind of document a piece of text is.
//
// Classification is semantic-first: an on-device model (Apple Foundation
// Models through kagaz-machelper, an MLX model, or a local Ollama model) reads
// the text and names a doctype. Deterministic keyword rules
// (doctypes.Catalog.Classify) are the offline fallback and remain the source of
// structured field extraction. This deliberately replaces an earlier
// regex-only design that was an unbounded maintenance treadmill.
//
// Four invariants hold on every path through this package:
//
//  1. No network at classify time. The Ollama backend re-validates that its
//     endpoint is loopback on every call and never trusts config alone.
//  2. Model output is never trusted. A returned doctype must exist in the
//     resolved catalog, and the category always comes from the catalog, never
//     from the model.
//  3. Every model tier can decline. All three -- Apple Foundation Models and
//     MLX through the Swift helpers, and Ollama here -- offer the model the
//     catalog with doctypes.Unclassified appended, and tell it in the prompt to
//     prefer that over a near miss. A model that cannot say "none of these"
//     does not stop giving wrong answers -- it stops being able to give a right
//     one, and reports the same high confidence either way. The two Swift
//     packages are independent by design, so this lives in each:
//     DocTypeCatalog.choices in machelper and machelper-mlx alike.
//  4. Graceful degradation, never a hard failure. A missing, broken, slow or
//     lying backend degrades to rules, and an unconfident rules answer degrades
//     to doctypes.Unclassified with zero confidence. Ingest is never failed by
//     a classifier problem.
//
// # What confidence means, and why min_confidence is still 0.5
//
// A model's confidence is only interpretable once it has an alternative to
// answering. While the schema offered nothing but real doctypes, every answer
// was forced, and a forced guess scored the same 0.90 as a genuine match --
// which made min_confidence a gate that nothing could fail. With
// doctypes.Unclassified in the schema, "not one of these" has somewhere to go,
// so a score above the gate now means the model both recognised the document
// and chose it over declining.
//
// The default gate stays at 0.5 and nothing is rescaled. The escape hatch
// removes the floor under the distribution rather than shifting it: genuine
// matches still land high, and the answers that used to be forced up now come
// back as a decline (0.0) or as a low score, on the correct side of the same
// threshold. Raising the gate would only start rejecting real matches.
//
// # min_confidence applies to the rules tier too
//
// classify.min_confidence gates the *rules* answer as well as the model's. At
// the default 0.5 that makes rules effectively pattern-or-strong-keyword only:
// doctypes.Catalog.Classify scores a lone weak keyword match below 0.5, so it
// becomes doctypes.Unclassified rather than a filed document. That is
// deliberate. A half-sure keyword guess is precisely what "never invent a
// category" (Global Constraint 8) exists to prevent, and an unclassified
// document is visible and fixable in a way that a confidently misfiled one is
// not. Lower classify.min_confidence to accept weaker rules matches.
package classify

import (
	"context"
	"math"
	"sort"
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

// maxText bounds how much document text is handed to a model backend, in
// bytes (not runes -- it is compared against len(), and clipping then backs up
// to the nearest rune boundary). A doctype is decided by the first page in
// practice, and an unbounded prompt is a reliable way to make a local model
// slow or to blow its context window. Rules always see the full text.
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
	// Fields are extracted structured values; nil when there are none. Every
	// model-supplied value in here has been found in the document text (see
	// grounder); values that could not be are in Dropped instead.
	Fields map[string]string
	// Dropped lists model-supplied fields that were withheld, with the reason,
	// so ingest can explain the absence rather than leave a silent hole.
	Dropped []DroppedField
	// Engine records which backend produced the accepted answer:
	// "apple" | "rules" | "mlx:<model basename>" | "ollama:<model>". It lands
	// in the sidecar's `classifier` field.
	Engine string
	// Degraded names a semantic tier that was asked and did not answer, when
	// the result therefore came from the tier below it. Nil is the normal
	// case, including a model that read the document and declined it.
	//
	// It exists for the same reason as Dropped: so a caller can explain an
	// absence rather than leave a silent hole. Without it, "unclassified"
	// reads as a statement about the document -- kagaz looked and could not
	// tell -- when it may mean nothing looked at all.
	Degraded *Degradation
}

// Degradation is a tier that failed, in its own words.
//
// Reason is the backend's error text, not a category: a timeout, a crashed
// helper and a malformed answer are different problems with different fixes,
// and flattening them into "unavailable" is what made the original silence
// hard to diagnose.
type Degradation struct {
	// Engine is the tier that failed: "apple", "mlx", "ollama".
	Engine string `json:"engine"`
	// Reason is the error the backend returned.
	Reason string `json:"reason"`
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

// declined reports whether a backend answered doctypes.Unclassified -- "none of
// the catalog fits". Every model tier offers that choice explicitly (the Apple
// helper adds it to the guided-generation schema, Ollama to its system prompt),
// so it is a real answer and must be told apart from a hallucinated doctype:
// both fail validate, but only one of them is the model behaving well.
func declined(res Result) bool {
	return config.Slug(strings.TrimSpace(res.DocType)) == doctypes.Unclassified
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

// mergeFields combines deterministic extraction with a model's fields, and is
// the one place a model-supplied field can become a fact about a document.
//
// Two rules decide what survives, and both exist because a wrong fact in a
// sidecar is worse than a missing one:
//
//   - Deterministic values win. doctypes.ExtractFields is a regex capture off
//     the real document text, so it is grounded by construction and is never
//     checked, never overwritten and never dropped. When the model returns the
//     same field name with a different value, the model's copy is dropped and
//     recorded with ReasonSuperseded; when the two agree there is nothing to
//     explain and nothing is recorded.
//   - Model values must be found in the text. Anything the model supplies for
//     a field the catalog has no template for is put through grounder against
//     the full text, and dropped with ReasonUngrounded if it is not there.
//
// text must be the unclipped Request.Text; see newGrounder.
func mergeFields(deterministic, model map[string]string, text string) (map[string]string, []DroppedField) {
	if len(deterministic) == 0 && len(model) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(deterministic)+len(model))
	for k, v := range deterministic {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}

	var g *grounder
	var dropped []DroppedField
	// Sorted so a dropped-field list is stable across runs and readable in a
	// preview: Go map iteration order is not.
	for _, k := range sortedKeys(model) {
		v := strings.TrimSpace(model[k])
		key := strings.TrimSpace(k)
		if key == "" || v == "" {
			continue
		}
		if have, ok := out[key]; ok {
			if !sameValue(have, v) {
				dropped = append(dropped, DroppedField{Field: key, Value: v, Reason: ReasonSuperseded})
			}
			continue
		}
		if g == nil {
			g = newGrounder(text)
		}
		if !g.grounded(v) {
			dropped = append(dropped, DroppedField{Field: key, Value: v, Reason: ReasonUngrounded})
			continue
		}
		out[key] = v
	}
	if len(out) == 0 {
		return nil, dropped
	}
	return out, dropped
}

// sortedKeys returns m's keys in a stable order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sameValue reports whether two renderings of a field value say the same
// thing, ignoring case, punctuation and spacing -- so a model echoing the
// rules tier's own capture back in a different shape is not reported as a
// disagreement the user has to read about.
func sameValue(a, b string) bool {
	ta, tb := tokenize(a), tokenize(b)
	if len(ta) != len(tb) {
		return false
	}
	for i := range ta {
		if ta[i] != tb[i] {
			return false
		}
	}
	return true
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
