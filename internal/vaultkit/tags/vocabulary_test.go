package tags

import (
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// mustConfig parses a minimal vault.yaml fragment, appending the required
// version/vault_root header so callers only need to supply the block under
// test.
func mustConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("version: 1\nvault_root: .\n" + yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func baseVocabConfig(t *testing.T) *config.Config {
	t.Helper()
	return mustConfig(t, `
people:
  - name: Alex Rao
    tag: alex
tags:
  companies: [acme]
  areas: [finance]
  fiscal_years: [fy2020]
`)
}

// TestVocabularyOrdinaryKinds covers Known/Kind/Validate/Unknown for the
// non-fiscal parts of the vocabulary: people, companies, areas and lifecycle
// tags, plus the default-lifecycle fallback.
func TestVocabularyOrdinaryKinds(t *testing.T) {
	cfg := baseVocabConfig(t)
	v := NewVocabulary(cfg)

	cases := []struct {
		tag  string
		kind Kind
	}{
		{"alex", KindPerson},
		{"acme", KindCompany},
		{"finance", KindArea},
		{"fy2020", KindFiscalYear}, // explicit entry
		{"active", KindLifecycle},  // default lifecycle tag
		{"ALEX", KindPerson},       // case-insensitive
		{"  acme  ", KindCompany},  // whitespace-insensitive
		{"nonexistent-vendor", KindUnknown},
	}
	for _, c := range cases {
		if got := v.Kind(c.tag); got != c.kind {
			t.Errorf("Kind(%q) = %q, want %q", c.tag, got, c.kind)
		}
		if got := v.Known(c.tag); got != (c.kind != KindUnknown) {
			t.Errorf("Known(%q) = %v, want %v", c.tag, got, c.kind != KindUnknown)
		}
	}
}

func TestVocabularyValidateAndUnknown(t *testing.T) {
	cfg := baseVocabConfig(t)
	v := NewVocabulary(cfg)

	if err := v.Validate([]string{"alex", "acme", "active"}); err != nil {
		t.Errorf("Validate of all-known tags: %v", err)
	}

	err := v.Validate([]string{"alex", "made-up-tag"})
	if err == nil {
		t.Fatal("Validate did not reject an out-of-vocabulary tag")
	}
	if !strings.Contains(err.Error(), "made-up-tag") {
		t.Errorf("Validate error %q does not name the offending tag", err.Error())
	}

	unk := v.Unknown([]string{"alex", "made-up-tag", "acme"})
	if len(unk) != 1 || unk[0] != "made-up-tag" {
		t.Errorf("Unknown = %v, want [made-up-tag]", unk)
	}
}

func TestVocabularyOfKindAndAll(t *testing.T) {
	cfg := baseVocabConfig(t)
	v := NewVocabulary(cfg)

	companies := v.OfKind(KindCompany)
	if len(companies) != 1 || companies[0] != "acme" {
		t.Errorf("OfKind(company) = %v, want [acme]", companies)
	}

	// OfKind(fiscal-year) must reflect only the explicitly configured
	// fiscal-year tags, not the thousands of years the calendar could
	// generate -- otherwise every lint/INDEX.md listing of the vocabulary
	// balloons with entries nobody wrote.
	fy := v.OfKind(KindFiscalYear)
	if len(fy) != 1 || fy[0] != "fy2020" {
		t.Errorf("OfKind(fiscal-year) = %v, want [fy2020]", fy)
	}

	all := v.All()
	if len(all) == 0 {
		t.Fatal("All() returned nothing")
	}
}

// TestVocabularyAutoAcceptsCalendarFiscalYear pins the papercut fix: a fresh
// vault with an empty tags.fiscal_years must still accept the fiscal-year tag
// its own fycal calendar would generate, and must classify it as
// KindFiscalYear (not KindUnknown) so lint and INDEX.md group it correctly.
func TestVocabularyAutoAcceptsCalendarFiscalYear(t *testing.T) {
	cfg := mustConfig(t, "") // tags.fiscal_years is empty on a fresh vault
	v := NewVocabulary(cfg)

	if got := v.Kind("fy2026"); got != KindFiscalYear {
		t.Errorf("Kind(fy2026) = %q, want %q", got, KindFiscalYear)
	}
	if !v.Known("fy2026") {
		t.Error("Known(fy2026) = false, want true")
	}
	if err := v.Validate([]string{"fy2026"}); err != nil {
		t.Errorf("Validate([fy2026]) = %v, want nil", err)
	}

	// Archival documents at both ends of a plausible human paper trail must
	// tag cleanly too -- a property deed from decades ago, a pension
	// statement decades out.
	for _, tag := range []string{"fy1998", "fy2050"} {
		if !v.Known(tag) {
			t.Errorf("Known(%q) = false, want true (archival fiscal year)", tag)
		}
	}
}

// TestVocabularyExplicitFiscalYearStillWorks makes sure an explicit
// tags.fiscal_years entry is unaffected by the auto-accept path, including
// for a year outside the calendar's plausible range.
func TestVocabularyExplicitFiscalYearStillWorks(t *testing.T) {
	cfg := mustConfig(t, "tags:\n  fiscal_years: [fy1850]\n")
	v := NewVocabulary(cfg)
	if got := v.Kind("fy1850"); got != KindFiscalYear {
		t.Errorf("Kind(fy1850) = %q, want %q (explicit entry)", got, KindFiscalYear)
	}
}

// TestVocabularyFiscalYearLabelFormats exercises the label_format variants a
// vault can configure: the calendar-year default, and two split-year
// spellings. The auto-accept path must derive the accepted shape from the
// calendar rather than hardcoding a regex for one spelling.
func TestVocabularyFiscalYearLabelFormats(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		accept string
	}{
		{"calendar year default", "", "fy2026"},
		{"split year default (start_month 4)", "fiscal_year:\n  start_month: 4\n", "fy25-26"},
		{"custom split spelling", "fiscal_year:\n  start_month: 4\n  label_format: \"FY {yyyy1}-{yy2}\"\n", "fy2025-26"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := mustConfig(t, c.yaml)
			v := NewVocabulary(cfg)
			if !v.Known(c.accept) {
				t.Errorf("Known(%q) = false, want true for %s", c.accept, c.name)
			}
			if got := v.Kind(c.accept); got != KindFiscalYear {
				t.Errorf("Kind(%q) = %q, want %q", c.accept, got, KindFiscalYear)
			}
		})
	}
}

// TestVocabularyRejectsFiscalLookalikes pins the other half of the fix: a tag
// that merely looks fiscal-ish, but that fycal.Calendar could never actually
// produce, must stay out of the vocabulary. Otherwise the auto-accept path
// degrades into a rubber stamp for anything shaped like "fy<digits>".
func TestVocabularyRejectsFiscalLookalikes(t *testing.T) {
	cfg := mustConfig(t, "") // calendar-year default, "FY {yyyy1}" -> "fy2026"
	v := NewVocabulary(cfg)

	rejects := []string{
		"fy20267",             // wrong digit count
		"fy-2026",             // the pre-fix dash spelling Tag() no longer emits
		"financial-year-2026", // unrelated spelling
		"fy202a",              // not numeric
		"2026",                // missing the fy prefix
		"fy25-26",             // split-year spelling on a calendar-year vault
	}
	for _, tag := range rejects {
		if v.Known(tag) {
			t.Errorf("Known(%q) = true, want false (not producible by this vault's calendar)", tag)
		}
		if got := v.Kind(tag); got != KindUnknown {
			t.Errorf("Kind(%q) = %q, want %q", tag, got, KindUnknown)
		}
	}
}

// TestVocabularyCalendarFiscalYearIsPrecomputed pins the cost characteristic
// of the auto-accept path: NewVocabulary must render the ~300-year calendar
// sweep once, so Kind() is a map lookup rather than a loop that re-renders
// fycal.Year.Tag() on every call. Kind() runs on every tag of every document
// that lint and search walk, so a per-call sweep would turn a 10,000-document
// vault into millions of Year.Tag() calls.
//
// The old, unmemoized implementation looped from minCalendarFiscalYear to
// maxCalendarFiscalYear and called Year.Label()/Year.Tag() (each of which
// allocates a strings.Builder and a strings.Replacer pass) on every miss, so
// it cost on the order of a thousand allocations per Kind() call. A
// precomputed map lookup costs a small, fixed handful. testing.AllocsPerRun
// distinguishes the two cleanly without depending on wall-clock timing.
func TestVocabularyCalendarFiscalYearIsPrecomputed(t *testing.T) {
	cfg := mustConfig(t, "") // calendar-year default; fiscal_years left empty
	v := NewVocabulary(cfg)

	if got := v.Kind("fy2026"); got != KindFiscalYear {
		t.Fatalf("Kind(fy2026) = %q, want %q (precondition)", got, KindFiscalYear)
	}

	const budget = 20 // generous headroom over a map lookup; nowhere near ~300 Tag() renders
	allocs := testing.AllocsPerRun(100, func() {
		v.Kind("fy2026")
	})
	if allocs > budget {
		t.Errorf("Kind(fy2026) allocated %.1f times per call, want <= %d; "+
			"looks like the calendar sweep runs on every call instead of being "+
			"precomputed in NewVocabulary", allocs, budget)
	}
}

// BenchmarkVocabularyKindCalendarFiscalYear is a companion to the allocation
// test above: run with `go test -bench=CalendarFiscalYear -benchmem` to see
// the absolute cost, which should look like a single map lookup rather than a
// sweep over the whole calendar range.
func BenchmarkVocabularyKindCalendarFiscalYear(b *testing.B) {
	cfg, err := config.Parse([]byte("version: 1\nvault_root: .\n"))
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	v := NewVocabulary(cfg)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v.Kind("fy2026")
	}
}
