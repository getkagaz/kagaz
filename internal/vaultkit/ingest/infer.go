package ingest

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/conventions"
)

// maxMatchText bounds how much extracted text owner inference scans. Owner
// names appear in a document's addressee block, not on page 300, and an
// unbounded scan of a large OCR result is work with no payoff.
const maxMatchText = 256 * 1024

// ---------------------------------------------------------------------------
// Owner inference
// ---------------------------------------------------------------------------

// inferOwners matches the vault's configured people against the source file
// name and the extracted text, in that order of preference.
//
// Three things can match, in descending strength:
//
//  1. the person's full display name ("Alex Rao");
//  2. the person's tag ("alex-rao"), which a user may have put in a filename;
//  3. the person's given name ("Alex") -- but only when that given name belongs
//     to exactly one configured person. Family members share a surname, so a
//     surname never matches on its own; a shared given name matches nobody,
//     because guessing between two people is worse than asking.
//
// Matching is on word boundaries over a normalised form, so "Alexandra" does
// not match "Alex" and "alex-rao" in a filename matches "Alex Rao".
//
// No match means no owner, which places the document in the category's shared
// or unowned folder. That is deliberate: an unowned document is visible and
// easy to correct, a wrongly-owned one disappears into somebody else's folder.
func inferOwners(cfg *config.Config, path, text string) ([]string, []Reason) {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	haystacks := []struct {
		norm   string
		source string
		where  string
	}{
		{normalizeForMatch(stem), SourceFilename, "the file name"},
		{normalizeForMatch(clip(text, maxMatchText)), SourceText, "the document text"},
	}

	unique := uniqueGivenNames(cfg)

	var owners []string
	var why []Reason
	for _, person := range cfg.People {
		candidates := []struct {
			token  string
			source string
			label  string
		}{
			{person.Name, "", fmt.Sprintf("the name %q", person.Name)},
			{strings.ReplaceAll(person.Tag, "-", " "), SourceTag, fmt.Sprintf("the tag %q", person.Tag)},
		}
		if given, ok := unique[person.Tag]; ok {
			candidates = append(candidates, struct {
				token  string
				source string
				label  string
			}{given, SourceGivenName, fmt.Sprintf("the given name %q, which belongs to exactly one person in this vault", given)})
		}

		matched := false
		for _, hay := range haystacks {
			for _, c := range candidates {
				token := normalizeForMatch(c.token)
				if token == "" || !containsWord(hay.norm, token) {
					continue
				}
				source := c.source
				if source == "" {
					source = hay.source
				}
				owners = append(owners, person.Name)
				why = append(why, Reason{
					Value:  person.Name,
					Source: source,
					Detail: fmt.Sprintf("owner %s: %s contains %s", person.Name, hay.where, c.label),
				})
				matched = true
				break
			}
			if matched {
				break
			}
		}
	}

	if len(owners) == 0 {
		names := make([]string, 0, len(cfg.People))
		for _, p := range cfg.People {
			names = append(names, p.Name)
		}
		detail := "no owner: none of the vault's people were named in the file name or the document text, so it is filed as shared/unowned"
		if len(names) > 0 {
			detail = fmt.Sprintf("no owner: none of %s were named in the file name or the document text, so it is filed as shared/unowned", strings.Join(names, ", "))
		}
		return nil, []Reason{{Value: "", Source: SourceNone, Detail: detail}}
	}
	return owners, why
}

// uniqueGivenNames maps a person's tag to their given name, but only for given
// names that are unique across the vault and long enough not to be noise.
func uniqueGivenNames(cfg *config.Config) map[string]string {
	count := map[string]int{}
	given := map[string]string{}
	for _, p := range cfg.People {
		fields := strings.Fields(p.Name)
		if len(fields) < 2 {
			// A single-word name is already covered by the full-name match.
			continue
		}
		g := strings.ToLower(fields[0])
		if len([]rune(g)) < 3 {
			continue
		}
		count[g]++
		given[p.Tag] = fields[0]
	}
	out := map[string]string{}
	for tag, g := range given {
		if count[strings.ToLower(g)] == 1 {
			out[tag] = g
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Year inference
// ---------------------------------------------------------------------------

// yearFieldOrder is the order in which extracted date fields are trusted for
// the document's year.
//
// Expiry-style fields are deliberately absent: a passport that expires in 2034
// was not issued in 2034, and a due date can fall in the following year. Only
// fields that describe when the document itself was made are used.
var yearFieldOrder = []string{
	"date",
	"invoice_date",
	"issue_date",
	"statement_date",
	"effective_date",
	"pay_period",
	"tax_year",
}

var yearRe = regexp.MustCompile(`\b(19[0-9]{2}|20[0-9]{2}|21[0-9]{2})\b`)

// inferYear takes the year from an extracted date field, else from the file's
// modification time.
//
// The mtime fallback is explicitly labelled a guess in its Reason, because it
// very often is one: a 2019 policy scanned today has a 2026 mtime, and the
// only thing standing between that and a misfiled document is the user reading
// the explanation before approving.
func inferYear(fields map[string]string, modTime time.Time) (int, Reason) {
	for _, name := range yearFieldOrder {
		v, ok := fields[name]
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		if y, ok := yearIn(v); ok {
			return y, Reason{
				Value:  strconv.Itoa(y),
				Source: SourceField,
				Detail: fmt.Sprintf("year %d: from the extracted %s field (%q)", y, name, strings.TrimSpace(v)),
			}
		}
	}
	y := modTime.Year()
	return y, Reason{
		Value:  strconv.Itoa(y),
		Source: SourceModTime,
		Detail: fmt.Sprintf("year %d: a guess from the file's modification date (%s) -- no date was extracted from the document, so correct this if the document is older than the file", y, modTime.Format("2006-01-02")),
	}
}

// yearIn pulls the first four-digit year out of a value.
func yearIn(v string) (int, bool) {
	m := yearRe.FindString(v)
	if m == "" {
		return 0, false
	}
	y, err := strconv.Atoi(m)
	if err != nil {
		return 0, false
	}
	return y, true
}

// ---------------------------------------------------------------------------
// Identifier inference
// ---------------------------------------------------------------------------

// identifierFieldOrder ranks extracted fields as identifiers, strongest first.
//
// A named counterparty beats a reference number, because "Acme-Corp" in a
// filename is worth more to a human scanning a folder than "INV-88213". Within
// the numbers, the ones that identify an ongoing relationship (an account, a
// policy) beat the ones that identify a single document.
var identifierFieldOrder = []string{
	"issuer",
	"merchant",
	"vendor",
	"supplier",
	"company",
	"employer",
	"provider",
	"bank",
	"counterparty",
	"account_number",
	"policy_number",
	"registration_number",
	"licence_number",
	"passport_number",
	"visa_number",
	"employee_id",
	"invoice_number",
	"receipt_number",
	"po_number",
	"claim_number",
	"booking_reference",
	"ticket_number",
}

// filenameNoise are stem tokens that identify nothing: scanner defaults,
// download artefacts and generic words.
var filenameNoise = map[string]bool{
	"scan": true, "scanned": true, "scans": true, "img": true, "image": true,
	"photo": true, "pic": true, "doc": true, "document": true, "file": true,
	"copy": true, "final": true, "new": true, "untitled": true, "download": true,
	"downloads": true, "pdf": true, "compressed": true, "merged": true,
	"page": true, "pages": true, "attachment": true, "unnamed": true,
}

// UnknownIdentifier is used when nothing usable could be inferred. It is a
// real, visible value rather than an empty one, because the filename pattern
// requires an identifier and a blank there would fail the whole proposal.
const UnknownIdentifier = "Untitled"

// rankableFields keys the extracted fields the way identifierFieldOrder names
// them, so a field is found however the extractor happened to spell it.
//
// Field names do not arrive in one shape. The catalog's regex extraction emits
// snake_case ("policy_number"), while a model reporting the fields it found
// returns them as the document writes them ("Policy Number", "Issuer"). The
// lookup used to be a plain map index, so the second kind was silently
// invisible: a correctly extracted issuer fell through to the file name and
// came back as "Untitled". Losing a fact the document actually stated is worse
// than any ranking mistake this table could make.
//
// The last spelling wins on collision, which is arbitrary but only reachable
// when one extractor emitted the same field twice under different spellings.
func rankableFields(fields map[string]string) map[string]string {
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out[normalizeFieldName(k)] = v
	}
	return out
}

// normalizeFieldName reduces a field name to lower snake_case: "Policy Number",
// "policy-number" and "POLICY_NUMBER" all become "policy_number".
func normalizeFieldName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	underscore := true // leading separators are dropped
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			underscore = false
			continue
		}
		if !underscore {
			b.WriteByte('_')
			underscore = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// identifierPlaceholders are values that name the absence of an answer. A model
// asked for an issuer it cannot find will often supply one of these rather than
// nothing, and filing it would state as fact that the document identifies
// itself as "Unknown".
var identifierPlaceholders = map[string]bool{
	"unknown": true, "unknwon": true, "none": true, "n a": true, "na": true,
	"nil": true, "null": true, "untitled": true, "unnamed": true,
	"not specified": true, "not stated": true, "not provided": true,
	"not applicable": true, "unspecified": true, "unavailable": true,
	"no issuer": true, "issuer": true, "tbd": true, "xxx": true,
}

// trimTrailingDoctype removes the doctype when it trails a candidate
// identifier, and only then.
//
// The doctype is already its own segment of the filename, so an "issuer" of
// "Electricity Bill" on a bill would produce
// "Bill_Alex-Rao_Electricity-Bill_2026" -- spending the one free-text segment
// repeating the segment before it. Models produce these readily, naming the
// kind of document when asked who issued it.
//
// The trailing restriction is what makes this safe, and dropping the doctype's
// words individually is not. "Insurance" and "Bank" are doctype words AND the
// ends of real company names: word-by-word removal turns "Northwind Insurance"
// into "Northwind", losing the issuer's actual name on a correct extraction,
// which is a worse outcome than the repetition it set out to fix. Matching the
// whole phrase at the end leaves "Northwind Insurance" alone (it does not end
// with "insurance policy") while still catching "Electricity Bill" and
// "Northwind Insurance Policy".
//
// A leading doctype is left alone too: "Passport Office" is the issuer of a
// passport, not a restatement of it.
func trimTrailingDoctype(candidate, docType string) string {
	phrase := strings.Fields(normalizeForMatch(conventions.TitleCase(docType)))
	if len(phrase) == 0 {
		phrase = strings.Fields(normalizeForMatch(docType))
	}
	if len(phrase) == 0 {
		return candidate
	}

	tokens := strings.Fields(candidate)
	if len(tokens) < len(phrase) {
		return candidate
	}
	tail := tokens[len(tokens)-len(phrase):]
	for i, want := range phrase {
		if normalizeForMatch(tail[i]) != want {
			return candidate
		}
	}
	return strings.Join(tokens[:len(tokens)-len(phrase)], " ")
}

// refuseIdentifier reports why a candidate identifier is not usable, or "" when
// it is.
//
// A placeholder is an extractor saying, in words, that it does not know.
// Filing one would state as fact that the document identifies itself as
// "Unknown".
func refuseIdentifier(candidate string) string {
	norm := normalizeForMatch(candidate)
	if norm == "" {
		return "it contains nothing usable"
	}
	if identifierPlaceholders[norm] {
		return "it names the absence of an answer rather than an issuer"
	}
	return ""
}

// inferIdentifier takes the identifier from the strongest extracted issuer
// field, else from a cleaned version of the source filename.
//
// The filename fallback strips the things a filename carries that are not an
// identifier: scanner noise, date stamps, the doctype word (already a separate
// field), and the owners' names (also already a separate field), so
// "scan_2024-03-02_alex_acme corp invoice.pdf" yields "Acme Corp" rather than
// repeating half the filename back into the new one.
func inferIdentifier(fields map[string]string, path, docType string, owners []string) (string, Reason) {
	byRank := rankableFields(fields)
	for _, name := range identifierFieldOrder {
		v := strings.TrimSpace(byRank[name])
		if v == "" {
			continue
		}
		if why := refuseIdentifier(v); why != "" {
			// Skipped, not returned: a weaker field is still a fact from the
			// document, and beats anything cleaned out of the file name.
			continue
		}
		trimmed := strings.TrimSpace(trimTrailingDoctype(v, docType))
		if trimmed == "" {
			continue // the field only restated the doctype
		}
		if trimmed != v {
			return trimmed, Reason{
				Value:  trimmed,
				Source: SourceField,
				Detail: fmt.Sprintf(
					"identifier %q: from the extracted %s field %q, with the doctype word dropped so the name does not repeat it",
					trimmed, name, v),
			}
		}
		return v, Reason{
			Value:  v,
			Source: SourceField,
			Detail: fmt.Sprintf("identifier %q: from the extracted %s field", v, name),
		}
	}

	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	cleaned := cleanStem(stem, docType, owners)
	if cleaned == "" {
		return UnknownIdentifier, Reason{
			Value:  UnknownIdentifier,
			Source: SourceNone,
			Detail: fmt.Sprintf("identifier %q: no issuer field was extracted and the file name %q contained nothing usable -- set one yourself before approving", UnknownIdentifier, stem),
		}
	}
	return cleaned, Reason{
		Value:  cleaned,
		Source: SourceFilename,
		Detail: fmt.Sprintf("identifier %q: no issuer field was extracted, so it was cleaned from the file name %q", cleaned, stem),
	}
}

// cleanStem reduces a filename stem to the part that identifies the document.
func cleanStem(stem, docType string, owners []string) string {
	drop := map[string]bool{}
	for _, w := range strings.Fields(normalizeForMatch(conventions.TitleCase(docType))) {
		drop[w] = true
	}
	for _, o := range owners {
		for _, w := range strings.Fields(normalizeForMatch(o)) {
			drop[w] = true
		}
	}

	var kept []string
	for _, tok := range strings.Fields(normalizeForMatch(stem)) {
		switch {
		case filenameNoise[tok], drop[tok]:
			continue
		case isDateish(tok):
			continue
		}
		kept = append(kept, tok)
	}
	if len(kept) == 0 {
		return ""
	}
	for i, w := range kept {
		kept[i] = upperFirst(w)
	}
	return strings.Join(kept, " ")
}

// upperFirst capitalises the first rune. It is rune-aware because a stem token
// can begin with a multi-byte character, where slicing the first byte would
// produce invalid UTF-8 in a filename.
func upperFirst(s string) string {
	for i, r := range s {
		return string(unicode.ToUpper(r)) + s[i+utf8.RuneLen(r):]
	}
	return s
}

// isDateish reports whether a stem token is a date fragment rather than part of
// an identifier: a bare year, a two-digit day or month, or a run of digits the
// length of a compact date stamp.
func isDateish(tok string) bool {
	for _, r := range tok {
		if r < '0' || r > '9' {
			return false
		}
	}
	switch len(tok) {
	case 1, 2, 4, 6, 8:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Matching helpers
// ---------------------------------------------------------------------------

// latinFolds maps Latin letters carrying a diacritic to their unaccented
// form, so a name typed with accents in vault.yaml still matches the same name
// typed without them (or mangled by OCR), which is common in both directions.
//
// Only Latin script is folded. Every other script -- Cyrillic, Greek, Arabic,
// Devanagari, Han, Hangul, Kana -- is kept verbatim by normalizeForMatch, which
// is the important half: Kagaz is a global tool, and an earlier version of this
// function dropped every non-ASCII letter, which turned wholly non-Latin names
// into the empty string (matching nothing, ever) and "Anaïs Dupont" into
// "ana s dupont", which word-matched a configured "Ana Dupont" and filed the
// document into the wrong person's folder with a confident explanation.
var latinFolds = buildFolds()

func buildFolds() map[rune]string {
	groups := []struct{ folded, runes string }{
		{"a", "àáâãäåāăą"},
		{"ae", "æ"},
		{"c", "çćĉċč"},
		{"d", "ďđð"},
		{"e", "èéêëēĕėęě"},
		{"g", "ĝğġģ"},
		{"h", "ĥħ"},
		{"i", "ìíîïĩīĭįı"},
		{"ij", "ĳ"},
		{"j", "ĵ"},
		{"k", "ķĸ"},
		{"l", "ĺļľŀł"},
		{"n", "ñńņňŋ"},
		{"o", "òóôõöøōŏő"},
		{"oe", "œ"},
		{"r", "ŕŗř"},
		{"s", "śŝşš"},
		{"ss", "ß"},
		{"t", "ţťŧ"},
		{"th", "þ"},
		{"u", "ùúûüũūŭůűų"},
		{"w", "ŵ"},
		{"y", "ýÿŷ"},
		{"z", "źżž"},
	}
	out := map[rune]string{}
	for _, g := range groups {
		for _, r := range g.runes {
			out[r] = g.folded
		}
	}
	return out
}

// normalizeForMatch lowercases, folds Latin diacritics, and reduces every run
// of non-letter, non-digit characters to one space, so "Alex_Rao-2024",
// "ALEX RAO 2024" and "Anaïs" / "Anais" compare equal on the parts that matter.
//
// Letters outside Latin are preserved rather than discarded: dropping them
// would make a non-Latin name normalize to the empty string, which cannot match
// and cannot be diagnosed.
func normalizeForMatch(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true
	for _, r := range strings.ToLower(s) {
		if folded, ok := latinFolds[r]; ok {
			b.WriteString(folded)
			space = false
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// containsWord reports whether needle occurs in haystack on word boundaries.
// Both must already be normalised.
func containsWord(haystack, needle string) bool {
	if needle == "" || haystack == "" {
		return false
	}
	padded := " " + haystack + " "
	return strings.Contains(padded, " "+needle+" ")
}

// clip truncates s to at most n bytes, on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	// Drop a trailing partial rune so the result is never invalid UTF-8. At
	// most utf8.UTFMax-1 bytes are shed.
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
