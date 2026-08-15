package tags

import (
	"fmt"
	"sort"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
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

// Vocabulary is the controlled tag set for a vault. A tag outside it is a lint
// violation: an uncontrolled vocabulary makes saved searches unreliable, which
// defeats the point of tagging at all.
type Vocabulary struct {
	kinds map[string]Kind
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
	return v
}

// Kind returns the vocabulary kind of tag, or KindUnknown.
func (v *Vocabulary) Kind(tag string) Kind {
	if k, ok := v.kinds[normalizeOne(tag)]; ok {
		return k
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
