package index

import (
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/search"
)

// templateSource is docs/AGENTS.template.md, embedded so that a `kagaz` binary
// renders AGENTS.md with no repository checkout present.
//
// It is a byte-for-byte copy of docs/AGENTS.template.md rather than the file
// itself because go:embed cannot reach outside its own package directory. The
// copy is not allowed to drift: TestTemplateMatchesDocs in this package fails
// if the two files differ, which is the only thing standing between "the docs
// document the contract" and "the docs used to document the contract".
//
//go:embed template/AGENTS.template.md
var templateSource string

// Placeholder tokens understood by the AGENTS.md renderer. They are the
// complete set docs/AGENTS.template.md uses; adding one is backwards
// compatible, renaming one is a breaking change to the template contract.
const (
	phVaultName       = "{KAGAZ_VAULT_NAME}"
	phFilenamePattern = "{KAGAZ_FILENAME_PATTERN}"
	phWordSeparator   = "{KAGAZ_WORD_SEPARATOR}"
	phFieldSeparator  = "{KAGAZ_FIELD_SEPARATOR}"
	phFiscalYearNote  = "{KAGAZ_FISCAL_YEAR_NOTE}"
	phCategoryTable   = "{KAGAZ_CATEGORY_TABLE}"
	phOwnerList       = "{KAGAZ_OWNER_LIST}"
	phTagVocabulary   = "{KAGAZ_TAG_VOCABULARY}"
	phDocTypeList     = "{KAGAZ_DOCTYPE_LIST}"
	phMDFindQueries   = "{KAGAZ_MDFIND_QUERIES}"
)

// Template returns the embedded AGENTS.md template verbatim.
func Template() string { return templateSource }

// leftoverRe finds a placeholder the renderer does not know about. A template
// that grows a token without the renderer growing with it must fail loudly:
// shipping `{KAGAZ_SOMETHING}` into a user's AGENTS.md is worse than not
// regenerating it at all.
var leftoverRe = regexp.MustCompile(`\{KAGAZ_[A-Z0-9_]+\}`)

// Agents renders AGENTS.md for this vault from the embedded template.
//
// Substitution is a plain string replacement per the template's own documented
// contract: the template deliberately has no loops or conditionals, so every
// multi-line placeholder is a pre-rendered Markdown fragment computed here,
// sorted, and free of timestamps.
func (g *Generator) Agents(tree *search.Tree) (string, error) {
	body := stripTemplateHeader(templateSource)

	replacements := []struct{ token, value string }{
		{phVaultName, g.VaultName()},
		{phFilenamePattern, g.cfg.Filename.Pattern},
		{phWordSeparator, g.cfg.Filename.WordSeparator},
		{phFieldSeparator, g.cfg.Filename.FieldSep},
		{phFiscalYearNote, g.fiscalNote()},
		{phCategoryTable, strings.TrimRight(g.categoryTable(tree.Documents), "\n")},
		{phOwnerList, strings.TrimRight(g.ownerList(), "\n")},
		{phTagVocabulary, strings.TrimRight(g.tagVocabulary(), "\n")},
		{phDocTypeList, strings.TrimRight(g.doctypeList(), "\n")},
		{phMDFindQueries, strings.TrimRight(g.mdfindQueries(), "\n")},
	}
	for _, r := range replacements {
		body = strings.ReplaceAll(body, r.token, r.value)
	}

	if left := leftoverRe.FindAllString(body, -1); len(left) > 0 {
		sort.Strings(left)
		return "", fmt.Errorf("AGENTS.md template uses placeholder(s) this build does not know: %s "+
			"(add them to internal/vaultkit/index/agents.go and document them in docs/AGENTS.template.md)",
			strings.Join(dedupe(left), ", "))
	}
	return strings.TrimLeft(body, "\n"), nil
}

// stripTemplateHeader removes the two parts of the template that exist for the
// docs site and for whoever implements this renderer, and that have no business
// in a user's vault: the Jekyll front matter and the leading HTML comment
// carrying the placeholder contract.
func stripTemplateHeader(src string) string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	if strings.HasPrefix(src, "---\n") {
		if end := strings.Index(src[4:], "\n---\n"); end >= 0 {
			src = src[4+end+len("\n---\n"):]
		}
	}
	trimmed := strings.TrimLeft(src, "\n \t")
	if strings.HasPrefix(trimmed, "<!--") {
		if end := strings.Index(trimmed, "-->"); end >= 0 {
			src = trimmed[end+len("-->"):]
		}
	}
	src = strings.TrimLeft(src, "\n")
	if !strings.HasSuffix(src, "\n") {
		src += "\n"
	}
	return src
}

// ownerList renders the vault's people, sorted by display name.
func (g *Generator) ownerList() string {
	if len(g.cfg.People) == 0 {
		return noneYet + "\n"
	}
	lines := make([]string, 0, len(g.cfg.People))
	for _, p := range g.cfg.People {
		lines = append(lines, fmt.Sprintf("- **%s** — tag `%s`, folder `%s`", p.Name, p.Tag, g.conv.Word(p.Name)))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// doctypeList renders every doctype in the resolved catalog with its category.
func (g *Generator) doctypeList() string {
	names := g.catalog.Names()
	if len(names) == 0 {
		return noneYet + "\n"
	}
	var b strings.Builder
	for _, n := range names {
		cat, _ := g.catalog.CategoryOf(n)
		fmt.Fprintf(&b, "- `%s` — `%s`\n", n, cat)
	}
	return b.String()
}

// dedupe removes repeats from a sorted list.
func dedupe(list []string) []string {
	var out []string
	for i, s := range list {
		if i == 0 || list[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}
