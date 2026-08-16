package tags

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/fycal"
)

// Kind classifies a tag by which part of the vocabulary it comes from.
type Kind string

// Tag kinds.
const (
	KindPerson     Kind = "person"
	KindCompany    Kind = "company"
	KindArea       Kind = "area"
	KindFiscalYear Kind = "fiscal-year"
	KindLifecycle  Kind = "lifecycle"
	KindUnknown    Kind = "unknown"
)

// minCalendarFiscalYear and maxCalendarFiscalYear bound the fiscal years whose
// generated tag a vault's own calendar auto-admits into the vocabulary (see
// Vocabulary.isCalendarFiscalTag). The range is deliberately wide and not tied
// to "now": a vault exists to hold a person's whole paper trail, so a
// decades-old property deed and a decades-out pension statement both need
// their fiscal-year tag to survive ingest just as well as this year's
// documents do. A window keyed to the current date would reintroduce the
// exact papercut this fix exists for, just deferred to the archival documents
// a vault is actually for. Widening the range costs almost nothing: a tag is
// only admitted if it exactly matches the *shape* fycal.Calendar itself would
// generate for that year, so the vocabulary is never weakened into a rubber
// stamp -- see isCalendarFiscalTag.
const (
	minCalendarFiscalYear = 1900
	maxCalendarFiscalYear = 2200
)

// Vocabulary is the controlled tag set for a vault. A tag outside it is a lint
// violation: an uncontrolled vocabulary makes saved searches unreliable, which
// defeats the point of tagging at all.
type Vocabulary struct {
	kinds map[string]Kind

	// calendarFiscal holds every tag this vault's fycal.Calendar could
	// generate across [minCalendarFiscalYear, maxCalendarFiscalYear],
	// rendered once here rather than on every Kind() lookup -- Kind() runs
	// on every tag of every document that lint and search walk, so a
	// per-call loop over ~300 years turns into millions of Year.Tag() calls
	// on a large vault. It is deliberately a set separate from kinds, not
	// folded in: OfKind/All must keep reflecting only the vocabulary a human
	// actually wrote (see their doc comments), or INDEX.md's fiscal-year
	// listing balloons with 301 synthetic years nobody configured. Kind()
	// consults kinds first specifically so an explicit tags.fiscal_years
	// entry is authoritative and this set can never appear to "override" it
	// -- the two just happen to agree when the strings match.
	calendarFiscal map[string]bool
}

// NewVocabulary builds the controlled vocabulary from vault.yaml.
func NewVocabulary(cfg *config.Config) *Vocabulary {
	v := &Vocabulary{kinds: map[string]Kind{}}
	for _, p := range cfg.People {
		v.kinds[normalizeOne(p.Tag)] = KindPerson
	}
	for _, c := range cfg.Tags.Companies {
		v.kinds[normalizeOne(c)] = KindCompany
	}
	for _, a := range cfg.Tags.Areas {
		v.kinds[normalizeOne(a)] = KindArea
	}
	for _, f := range cfg.Tags.FiscalYears {
		v.kinds[normalizeOne(f)] = KindFiscalYear
	}
	for _, l := range cfg.Tags.Lifecycle {
		v.kinds[normalizeOne(l)] = KindLifecycle
	}
	delete(v.kinds, "")

	cal := fycal.New(cfg.FiscalYear.StartMonth, cfg.FiscalYear.LabelFormat)
	v.calendarFiscal = make(map[string]bool, maxCalendarFiscalYear-minCalendarFiscalYear+1)
	for y := minCalendarFiscalYear; y <= maxCalendarFiscalYear; y++ {
		if t := normalizeOne(cal.YearStarting(y).Tag()); t != "" {
			v.calendarFiscal[t] = true
		}
	}
	return v
}

// Kind returns the vocabulary kind of tag, or KindUnknown.
func (v *Vocabulary) Kind(tag string) Kind {
	n := normalizeOne(tag)
	if k, ok := v.kinds[n]; ok {
		return k
	}
	if v.calendarFiscal[n] {
		return KindFiscalYear
	}
	return KindUnknown
}

// Known reports whether tag is in the vocabulary.
func (v *Vocabulary) Known(tag string) bool { return v.Kind(tag) != KindUnknown }

// Unknown returns the tags in list that are outside the vocabulary.
func (v *Vocabulary) Unknown(list []string) []string {
	var out []string
	for _, t := range Normalize(list) {
		if !v.Known(t) {
			out = append(out, t)
		}
	}
	return out
}

// Validate returns an error naming every tag outside the vocabulary.
func (v *Vocabulary) Validate(list []string) error {
	unknown := v.Unknown(list)
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("tag(s) not in the vault vocabulary: %s (add them to vault.yaml, or use --force)", strings.Join(unknown, ", "))
}

// OfKind returns every vocabulary tag of a given kind, sorted.
func (v *Vocabulary) OfKind(k Kind) []string {
	var out []string
	for t, kind := range v.kinds {
		if kind == k {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// All returns the whole vocabulary, sorted.
func (v *Vocabulary) All() []string {
	out := make([]string, 0, len(v.kinds))
	for t := range v.kinds {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Lifecycle returns the lifecycle tags present in list.
func (v *Vocabulary) Lifecycle(list []string) []string {
	var out []string
	for _, t := range Normalize(list) {
		if v.Kind(t) == KindLifecycle {
			out = append(out, t)
		}
	}
	return out
}
