package classify

import (
	"context"
	"errors"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
)

// Rules is the deterministic tier: catalog keyword and pattern matching plus
// regex field extraction. It needs no model, no helper and no network, so it is
// always available and is the floor every other backend falls back to.
//
// Its confidence is capped at 0.95 by doctypes.Catalog.Classify on purpose --
// rules never claim near-certainty -- and this package does not re-scale it.
type Rules struct {
	// Catalog is the fallback catalog used when a Request carries none.
	Catalog *doctypes.Catalog
}

// Name identifies the backend. It matches config.EngineRules.
func (r *Rules) Name() string { return config.EngineRules }

// Available is always true: rules are pure Go with no external dependency.
func (r *Rules) Available() bool { return true }

// Classify scores the text against the catalog and extracts the matched
// doctype's fields. An unmatched document comes back as doctypes.Unclassified
// with zero confidence rather than a guess.
func (r *Rules) Classify(_ context.Context, req Request) (Result, error) {
	cat := req.Catalog
	if cat == nil {
		cat = r.Catalog
	}
	if cat == nil {
		return Result{}, errors.New("rules: no doctype catalog resolved")
	}

	m := cat.Classify(req.Text)
	if m.DocType == "" || m.DocType == doctypes.Unclassified {
		return unclassified(), nil
	}
	return Result{
		DocType:    m.DocType,
		Category:   m.Category,
		Confidence: m.Confidence,
		Fields:     cat.ExtractFields(m.DocType, req.Text),
		Engine:     config.EngineRules,
	}, nil
}
