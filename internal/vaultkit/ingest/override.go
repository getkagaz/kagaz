package ingest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
)

// ClassifierHuman is the value recorded in a proposal's Classifier field, and
// therefore in the sidecar's `classifier` field, when a person stated the
// doctype instead of a classifier tier deciding it.
//
// It is deliberately not one of the engine names ("rules", "apple",
// "mlx:<model>", "ollama:<model>"): a sidecar is the record a user reads back
// months later to decide whether to trust a filing, and a human decision
// recorded as a model call is a lie in that record. Anything reading the
// sidecar can tell the two apart by this one value.
const ClassifierHuman = "human"

// Overrides are facts a person states on the command line instead of letting
// Kagaz infer them. They apply to every path in one invocation: one doctype per
// invocation, applied to all files given, which is what a triage view needs --
// it selects the rows that are all the same kind of document and calls once.
//
// The category is never overridable. It always comes from the resolved catalog
// (Global Constraint 8, in the human direction): a person may pick any real
// doctype, but may not invent one, and may not move a doctype into a category
// the vault's catalog does not put it in.
type Overrides struct {
	// DocType is the catalog doctype every named path is filed as. Empty means
	// classify normally.
	DocType string
	// Owners are the people every named path belongs to, given as display names
	// or tags. Empty means infer.
	Owners []string
	// Identifier is the identifier every named path is filed under. Empty means
	// infer.
	Identifier string
	// Year is the year every named path is filed under. Zero means infer.
	Year int
}

// Empty reports whether nothing was overridden.
func (o Overrides) Empty() bool {
	return o.DocType == "" && len(o.Owners) == 0 && o.Identifier == "" && o.Year == 0
}

// resolve validates the overrides against the vault and returns them in the
// canonical form the pipeline uses: the catalog's doctype name, and each
// owner's configured display name.
//
// Validation happens once, before any file is read, so a mistyped doctype costs
// a message rather than a batch of OCR.
func (o Overrides) resolve(cfg *config.Config, cat *doctypes.Catalog) (Overrides, error) {
	out := Overrides{Year: o.Year}

	if raw := strings.TrimSpace(o.DocType); raw != "" {
		name := config.Slug(raw)
		switch {
		case cat == nil:
			return Overrides{}, fmt.Errorf("ingest: no doctype catalog is resolved, so --set-doctype cannot be checked")
		case name == doctypes.Unclassified:
			return Overrides{}, fmt.Errorf("--set-doctype %s is not a filing decision: %s is what Kagaz says when it does not know. Name a real doctype, one of: %s",
				raw, doctypes.Unclassified, strings.Join(sampleNames(cat.Names(), name, 5), ", "))
		case !cat.Has(name):
			return Overrides{}, fmt.Errorf("--set-doctype %s is not a doctype in this vault's catalog. Did you mean: %s? "+
				"(add a doctypes: entry to vault.yaml to introduce a new one -- kagaz will not invent a category for a name it does not know)",
				raw, strings.Join(sampleNames(cat.Names(), name, 5), ", "))
		}
		out.DocType = name
	}

	for _, raw := range o.Owners {
		want := strings.TrimSpace(raw)
		if want == "" {
			continue
		}
		person, ok := cfg.Person(want)
		if !ok {
			return Overrides{}, fmt.Errorf("--set-owner %s is not a person in this vault. vault.yaml lists: %s",
				want, strings.Join(peopleNames(cfg), ", "))
		}
		if !hasName(out.Owners, person.Name) {
			out.Owners = append(out.Owners, person.Name)
		}
	}

	out.Identifier = strings.TrimSpace(o.Identifier)
	if o.Identifier != "" && out.Identifier == "" {
		return Overrides{}, fmt.Errorf("--set-identifier is blank; give the text the document should be filed under, or leave the flag off to infer one")
	}

	if o.Year != 0 && (o.Year < 1000 || o.Year > 9999) {
		return Overrides{}, fmt.Errorf("--set-year %d is not a four-digit year", o.Year)
	}
	return out, nil
}

// peopleNames lists the vault's configured people for an error message.
func peopleNames(cfg *config.Config) []string {
	if cfg == nil || len(cfg.People) == 0 {
		return []string{"(nobody -- add a people: block to vault.yaml)"}
	}
	out := make([]string, 0, len(cfg.People))
	for _, p := range cfg.People {
		if p.Tag != "" && !strings.EqualFold(p.Tag, p.Name) {
			out = append(out, fmt.Sprintf("%s (%s)", p.Name, p.Tag))
			continue
		}
		out = append(out, p.Name)
	}
	return out
}

// sampleNames picks up to n catalog names to show alongside a rejection: the
// closest matches first, so a typo is answered with the name that was meant,
// then whatever else fits, so the message is never just "no".
func sampleNames(names []string, want string, n int) []string {
	if len(names) == 0 {
		return []string{"(this vault's catalog is empty)"}
	}
	scored := make([]string, len(names))
	copy(scored, names)
	sort.SliceStable(scored, func(i, j int) bool {
		return nearness(scored[i], want) < nearness(scored[j], want)
	})
	if len(scored) > n {
		scored = scored[:n]
	}
	return scored
}

// nearness ranks a catalog name against what the user typed: lower is closer.
// A shared prefix or a containment beats raw edit distance, because the typical
// miss is a plural, an abbreviation or half a name.
func nearness(name, want string) int {
	switch {
	case name == want:
		return 0
	case want != "" && (strings.Contains(name, want) || strings.Contains(want, name)):
		return 1
	}
	return 2 + editDistance(name, want)
}

// editDistance is Levenshtein distance over runes, used only to order
// suggestions in an error message.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// hasName reports whether list already holds want.
func hasName(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
