package classify

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// DroppedField is a field a model proposed that was not written, with the
// reason. It exists so that dropping is never silent: ingest carries these
// through to its proposal preview, next to the inferences it already explains.
type DroppedField struct {
	// Field is the field name the model used.
	Field string `json:"field"`
	// Value is the value that was withheld.
	Value string `json:"value"`
	// Reason is one sentence explaining why it was withheld.
	Reason string `json:"reason"`
}

// Reasons a model field is withheld. They are constants so the CLI and tests
// can match on them without duplicating prose.
const (
	// ReasonUngrounded is the hallucination guard: the value could not be
	// found in the document text at all.
	ReasonUngrounded = "the value does not appear anywhere in the document text, so it would be a fact about the document that the document does not contain"
	// ReasonSuperseded is the rules-win rule: a regex capture off the real
	// text beats a model's paraphrase of the same field.
	ReasonSuperseded = "the rules tier extracted this field directly from the document text, and a regex capture of the real text is preferred over a model's value"
)

// grounder answers "does this value occur in the document text?" for the
// values one model returned about one document.
//
// # Why this exists
//
// A model asked to fill a schema will fill it. Given a document with no date
// and no reference number, the Apple Foundation Models backend returned
// date=2025-01-01 and document_number=12345 -- placeholder-shaped values
// invented to satisfy the schema. Those would have been written to the
// sidecar as facts, and `kagaz find` and any agent reading the vault would
// have believed them. A missing field is recoverable; a fabricated one is not.
//
// # The rule
//
// A model-supplied value is kept only if it can be found in the document
// text. The comparison tolerates the ways a model legitimately *renders* a
// value differently, and nothing else:
//
//  1. Token containment. Both sides are lowercased and cut into runs of
//     letters and digits, and the value's tokens must appear contiguously in
//     the text's. This absorbs case, whitespace, line breaks and punctuation:
//     "ACME  CORP." is grounded in "Acme Corp", and "INV-2026-4471" in
//     "Invoice Number: INV/2026/4471".
//
//  2. Numeric equality. A value that is a bare number (with optional currency
//     sign, thousands separators and decimals) is compared against every
//     number in the text after both are canonicalised -- separators removed
//     and trailing decimal zeros trimmed. This grounds "4800" in
//     "Rs. 4,800.00". It does not ground a number that is simply absent.
//
//  3. Date component equality. A value that parses as a date is reduced to a
//     year-month-day triple, as is every date-shaped span in the text, and the
//     value is grounded if any triple matches. This grounds "2026-03-11" in
//     "11 March 2026" -- the same day written differently -- while leaving
//     "2025-01-01" ungrounded in a text with no 2025 and no date at all.
//
// What it deliberately does not do is fuzzy matching. There is no edit
// distance, no partial-token or prefix match, no "close enough" numeric
// tolerance and no year-only fallback for dates. Every tier above is an
// exact match on a canonical form; only the rendering is negotiable. That is
// what keeps the invented values out: 12345 is not a token of the text, not a
// number in the text, and not a date in the text, so all three tiers reject
// it.
type grounder struct {
	tokens  []string
	numbers map[string]bool
	dates   map[ymd]bool
}

// newGrounder indexes the document text once, so a document's fields are all
// checked against one pass over it.
//
// text must be the *full* Request.Text, never Request.text(): the latter is
// clipped to maxText before it is handed to a model, and grounding against the
// clipped copy would drop a real value that the model read from the part that
// survived clipping only if it happened to fall in the same window. The full
// text is a superset of whatever any backend saw, so a value grounded in what
// the model was shown is always grounded here.
func newGrounder(text string) *grounder {
	return &grounder{
		tokens:  tokenize(text),
		numbers: numbersIn(text),
		dates:   datesIn(text),
	}
}

// grounded reports whether value occurs in the indexed text under the rule
// documented on grounder.
func (g *grounder) grounded(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if vt := tokenize(value); len(vt) > 0 && containsTokens(g.tokens, vt) {
		return true
	}
	if n, ok := canonicalNumber(value); ok && g.numbers[n] {
		return true
	}
	for d := range datesIn(value) {
		if g.dates[d] {
			return true
		}
	}
	return false
}

// Grounded reports whether value can be found in text: the check that decides
// whether a model-extracted field becomes a fact in a document's sidecar. See
// grounder for the exact rule and what formatting differences it tolerates.
func Grounded(text, value string) bool {
	return newGrounder(text).grounded(value)
}

// tokenize lowercases s and cuts it into runs of letters and digits, dropping
// everything else. Punctuation, currency symbols and whitespace become token
// boundaries rather than characters to compare, which is what makes the
// comparison indifferent to how a value was typeset.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// containsTokens reports whether want appears as a contiguous run inside have.
// Contiguity matters: "Acme Corp" must not be grounded by a text that says
// "Acme" in one paragraph and "Corp" in another.
func containsTokens(have, want []string) bool {
	if len(want) == 0 || len(want) > len(have) {
		return false
	}
	for i := 0; i+len(want) <= len(have); i++ {
		match := true
		for j := range want {
			if have[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// numberRe finds number-shaped spans in a document: digits with optional
// thousands separators and an optional decimal part.
var numberRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// valueNumberRe recognises a *value* that is nothing but a number: an optional
// currency symbol or minus sign, then digits with separators and decimals. A
// value carrying words ("invoice 12345") is not a number and is not allowed to
// be grounded numerically -- its words have to be in the text too.
var valueNumberRe = regexp.MustCompile(`^[\p{Sc}]?\s*-?\s*\d[\d, ]*(?:\.\d+)?$`)

// numbersIn indexes every number in text in canonical form.
func numbersIn(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range numberRe.FindAllString(text, -1) {
		if n, ok := canonicalNumber(m); ok {
			out[n] = true
		}
	}
	return out
}

// canonicalNumber reduces a numeric string to a comparable form: separators
// and spaces removed, trailing decimal zeros trimmed. "Rs. 4,800.00" is not a
// number (it has letters); "4,800.00", "4800" and "4 800.0" all canonicalise
// to "4800".
//
// It assumes '.' is the decimal separator and ',' groups thousands, which is
// the convention of every locale Kagaz's doctype catalog ships patterns for.
// A "4.800,00" rendering simply fails to match and the field is dropped --
// the safe direction.
func canonicalNumber(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !valueNumberRe.MatchString(s) {
		return "", false
	}
	neg := strings.Contains(s, "-")
	s = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == '.' {
			return r
		}
		return -1
	}, s)
	if s == "" {
		return "", false
	}
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "" {
		return "", false
	}
	if neg {
		s = "-" + s
	}
	return s, true
}

// ymd is a calendar date reduced to its components, which is the form two
// different renderings of the same day agree on.
type ymd struct{ y, m, d int }

var (
	// numericDateRe matches 2026-03-11, 11/03/2026, 11.3.26 and friends.
	numericDateRe = regexp.MustCompile(`(\d{1,4})[-/.](\d{1,2})[-/.](\d{1,4})`)
	// dayMonthNameRe matches "11 March 2026" and "11th Mar, 2026".
	dayMonthNameRe = regexp.MustCompile(`(?i)(\d{1,2})(?:st|nd|rd|th)?[\s,.]+([a-z]{3,9})\.?[\s,]+(\d{4})`)
	// monthNameDayRe matches "March 11, 2026" and "Mar 11 2026".
	monthNameDayRe = regexp.MustCompile(`(?i)([a-z]{3,9})\.?[\s,]+(\d{1,2})(?:st|nd|rd|th)?[\s,]+(\d{4})`)
)

// months maps the first three letters of an English month name to its number.
// Three letters is enough to be unambiguous and covers both "Mar" and "March".
var months = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

// datesIn extracts every date-shaped span in s as a set of candidate ymd
// triples. An ambiguous numeric date contributes *both* readings (11/03 is
// offered as 11 March and as 3 November), because the point is to decide
// whether the day the model reported is written in the document, not to settle
// which locale wrote it. Both readings come from digits that are really there,
// so neither can ground an invented date.
func datesIn(s string) map[ymd]bool {
	out := map[ymd]bool{}
	for _, m := range numericDateRe.FindAllStringSubmatch(s, -1) {
		a, b, c := atoi(m[1]), atoi(m[2]), atoi(m[3])
		if len(m[1]) == 4 {
			add(out, ymd{a, b, c}) // 2026-03-11
			continue
		}
		year := expandYear(c)
		add(out, ymd{year, b, a}) // day/month/year
		add(out, ymd{year, a, b}) // month/day/year
	}
	for _, m := range dayMonthNameRe.FindAllStringSubmatch(s, -1) {
		if mon, ok := monthNumber(m[2]); ok {
			add(out, ymd{atoi(m[3]), mon, atoi(m[1])})
		}
	}
	for _, m := range monthNameDayRe.FindAllStringSubmatch(s, -1) {
		if mon, ok := monthNumber(m[1]); ok {
			add(out, ymd{atoi(m[3]), mon, atoi(m[2])})
		}
	}
	return out
}

// add records a candidate date if its components are a possible calendar date.
func add(set map[ymd]bool, v ymd) {
	if v.y < 1000 || v.y > 9999 || v.m < 1 || v.m > 12 || v.d < 1 || v.d > 31 {
		return
	}
	set[v] = true
}

// monthNumber resolves an English month name or its three-letter abbreviation.
func monthNumber(name string) (int, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if len(name) < 3 {
		return 0, false
	}
	n, ok := months[name[:3]]
	return n, ok
}

// expandYear turns a two-digit year into a four-digit one. It only ever runs
// on digits taken from the text or the value, so it cannot invent a year that
// neither contains.
func expandYear(y int) int {
	if y < 100 {
		return 2000 + y
	}
	return y
}

// atoi parses digits that a regex already proved are digits.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
