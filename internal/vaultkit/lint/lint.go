// Package lint checks a vault against its own conventions and reports what it
// finds. It is the honesty layer: Kagaz's conventions are only worth anything
// if drift is visible.
//
// Two rules govern everything here:
//
//   - A finding is a report, not a failure. A vault accumulates files Kagaz did
//     not create, and lint's job is to name them, never to refuse to run.
//   - `--fix` repairs only what is provably right. If a repair would need a
//     guess — which of two `active` documents is the current one, what the
//     unreadable filename was supposed to say — the finding is reported and
//     left alone. Every repair that is applied goes through move.Engine and
//     writes a manifest, exactly like any other mutation.
package lint

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/conventions"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/search"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// Severity ranks a finding.
type Severity string

// Severities. Error means the vault's own guarantees are broken (a document is
// unfindable by its conventions, or a secret is sitting in a filename). Warning
// means a convention is not being followed but nothing is lost. Info is a note.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Rule ids. These are user-facing and appear in `kagaz lint --json`, in docs
// and in suppression discussions, so they are stable identifiers: renaming one
// is a breaking change, the same rule as the machelper JSON contract.
const (
	// RuleNameGrammar: the filename does not match the vault's grammar at all.
	RuleNameGrammar = "name-grammar"
	// RuleNameNormalization: the filename parses but is not what the grammar
	// would have rendered (wrong capitalisation, stray separators).
	RuleNameNormalization = "name-normalization"
	// RuleUnknownDocType: the filename's doctype is not in the resolved catalog.
	RuleUnknownDocType = "unknown-doctype"
	// RuleWrongFolder: the file is not in the folder its doctype's category and
	// the vault's layout put it in.
	RuleWrongFolder = "wrong-folder"
	// RuleUnknownTag: a Finder tag outside the controlled vocabulary.
	RuleUnknownTag = "unknown-tag"
	// RuleMissingLifecycleTag: no lifecycle tag, where vault.yaml requires one.
	RuleMissingLifecycleTag = "missing-lifecycle-tag"
	// RuleMultipleActive: more than one `active` document for a doctype+person
	// listed in lint.single_active_per_doctype_per_person.
	RuleMultipleActive = "multiple-active"
	// RulePasswordInFilename: a password-looking token in a filename.
	RulePasswordInFilename = "password-in-filename"
	// RuleStaleSidecar: the sidecar's source_sha256 no longer matches the file.
	RuleStaleSidecar = "stale-sidecar"
	// RuleOrphanSidecar: a sidecar whose document is not there.
	RuleOrphanSidecar = "orphan-sidecar"
)

// RuleDoc describes one rule for `kagaz lint --list-rules` and for the docs.
type RuleDoc struct {
	ID          string
	Severity    Severity
	Fixable     bool
	Description string
}

// Rules returns every rule, id-sorted, with whether `--fix` can ever repair it.
// "Fixable" here means the rule has a safe repair in some circumstances; an
// individual finding still reports its own Fixable, because most of these
// repairs depend on there being unambiguous evidence for them.
func Rules() []RuleDoc {
	out := []RuleDoc{
		{RuleNameGrammar, SeverityError, true, "filename does not match the vault's filename grammar; repairable only when the sidecar supplies every required field"},
		{RuleNameNormalization, SeverityWarning, true, "filename parses but is not the grammar's canonical rendering of its own facts"},
		{RuleUnknownDocType, SeverityWarning, false, "the filename's doctype is not in the resolved doctype catalog"},
		{RuleWrongFolder, SeverityWarning, true, "the file is not in the folder its category and layout require"},
		{RuleUnknownTag, SeverityWarning, false, "a Finder tag outside the vault's controlled vocabulary"},
		{RuleMissingLifecycleTag, SeverityWarning, true, "no lifecycle tag, where lint.require_lifecycle_tag is set; repairable only when the sidecar names one unambiguously"},
		{RuleMultipleActive, SeverityError, false, "more than one document tagged active for a doctype+person under lint.single_active_per_doctype_per_person"},
		{RulePasswordInFilename, SeverityError, false, "a password-looking token in a filename, where lint.forbid_passwords_in_filenames is set"},
		{RuleStaleSidecar, SeverityWarning, false, "the sidecar's source_sha256 does not match the document; re-extract with kagaz ingest --reindex"},
		{RuleOrphanSidecar, SeverityInfo, false, "a sidecar whose document is not present"},
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Repair is the exact change `--fix` would make. It exists so that a preview
// and the fix itself come from one description rather than two code paths.
type Repair struct {
	// MoveTo is the destination, relative to the vault root and
	// slash-separated. Empty when the repair is not a move.
	MoveTo string `json:"move_to,omitempty"`
	// AddTag is a Finder tag to add. Empty when the repair is not a tagging.
	AddTag string `json:"add_tag,omitempty"`
}

// Finding is one rule violation.
type Finding struct {
	// Rule is the stable rule id.
	Rule string `json:"rule"`
	// Severity ranks the finding.
	Severity Severity `json:"severity"`
	// Path is the offending file, relative to the vault root and
	// slash-separated. Relative on purpose: an absolute path is specific to one
	// machine and would make generated output and golden tests unstable.
	Path string `json:"path"`
	// Message explains the violation in one line.
	Message string `json:"message"`
	// Fixable reports whether `--fix` can repair this particular finding. It is
	// always Repair != nil.
	Fixable bool `json:"fixable"`
	// Repair describes the repair, when there is one.
	Repair *Repair `json:"repair,omitempty"`

	// AbsPath is the offending file's absolute path. Not serialized: it is for
	// the fixer, which has to touch the file.
	AbsPath string `json:"-"`
}

// Linter runs the rule engine over one vault.
type Linter struct {
	// Search walks the tree. Tests reach through it to substitute tag reading.
	Search *search.Searcher
	// Engine performs `--fix` moves. Nil means one is built from the vault
	// config on first use.
	Engine *move.Engine

	cfg     *config.Config
	conv    *conventions.Conventions
	catalog *doctypes.Catalog
	vocab   *tags.Vocabulary
}

// New builds a Linter for a vault.
func New(cfg *config.Config) (*Linter, error) {
	s, err := search.New(cfg)
	if err != nil {
		return nil, err
	}
	cat, err := doctypes.Resolve(cfg)
	if err != nil {
		return nil, err
	}
	return &Linter{
		Search:  s,
		cfg:     cfg,
		conv:    s.Conventions(),
		catalog: cat,
		vocab:   tags.NewVocabulary(cfg),
	}, nil
}

// Run walks the vault and returns every finding, sorted by path then rule id so
// that two runs over an unchanged tree produce byte-identical output.
func (l *Linter) Run(ctx context.Context) ([]Finding, error) {
	tree, err := l.Search.Scan(ctx)
	if err != nil {
		return nil, err
	}
	return l.Check(tree)
}

// Check applies the rules to an already-walked tree.
func (l *Linter) Check(tree *search.Tree) ([]Finding, error) {
	var findings []Finding
	for i := range tree.Documents {
		findings = append(findings, l.checkDocument(&tree.Documents[i])...)
	}
	findings = append(findings, l.checkSingleActive(tree)...)
	for _, side := range tree.OrphanSidecars {
		findings = append(findings, Finding{
			Rule:     RuleOrphanSidecar,
			Severity: SeverityInfo,
			Path:     side,
			AbsPath:  filepath.Join(l.cfg.VaultRoot, filepath.FromSlash(side)),
			Message:  "sidecar has no document; delete it yourself, or restore the document it describes",
		})
	}
	sortFindings(findings)
	return findings, nil
}

func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Path != f[j].Path {
			return f[i].Path < f[j].Path
		}
		return f[i].Rule < f[j].Rule
	})
}

// checkDocument runs every per-document rule.
func (l *Linter) checkDocument(doc *search.Document) []Finding {
	var out []Finding
	add := func(f Finding) {
		f.Path = doc.RelPath
		f.AbsPath = doc.Path
		f.Fixable = f.Repair != nil
		out = append(out, f)
	}

	if l.cfg.Lint.ForbidPasswordsInFilenames {
		if token, ok := passwordToken(doc.Name); ok {
			add(Finding{
				Rule:     RulePasswordInFilename,
				Severity: SeverityError,
				Message: fmt.Sprintf("filename contains a password-looking token %q; a secret belongs in the Keychain, never in a filename "+
					"(rename the file, then remove the secret from wherever it came from)", token),
			})
		}
	}

	switch {
	case !doc.Parsed:
		f := Finding{
			Rule:     RuleNameGrammar,
			Severity: SeverityError,
			Message:  fmt.Sprintf("filename does not match the vault grammar %q", l.cfg.Filename.Pattern),
		}
		if dst, ok := l.renameFromSidecar(doc); ok {
			f.Repair = &Repair{MoveTo: dst}
			f.Message += "; its sidecar supplies every field, so --fix can rename it to " + path.Base(dst)
		} else {
			f.Message += "; no sidecar supplies the missing facts, so --fix leaves it alone (rename it yourself, or re-ingest it)"
		}
		add(f)
	case !l.catalog.Has(doc.Doc.DocType):
		add(Finding{
			Rule:     RuleUnknownDocType,
			Severity: SeverityWarning,
			Message: fmt.Sprintf("doctype %q is not in this vault's catalog; add it under doctypes: in vault.yaml, or rename the file",
				doc.Doc.DocType),
		})
	default:
		out = append(out, l.checkPlacement(doc)...)
	}

	out = append(out, l.checkTags(doc)...)
	out = append(out, l.checkSidecar(doc)...)
	return out
}

// checkPlacement covers the two rules that share one canonical destination: the
// filename's own rendering and the folder it lives in. Both report MoveTo as
// the fully canonical path, and Fix collapses them into a single move.
func (l *Linter) checkPlacement(doc *search.Document) []Finding {
	want := doc.Doc
	cat, ok := l.catalog.CategoryOf(want.DocType)
	if !ok {
		return nil
	}
	want.Category = cat
	canonical, err := l.conv.Path(want)
	if err != nil {
		// The grammar cannot render these facts (a required field is empty).
		// That is not a placement problem; nothing safe can be said here.
		return nil
	}
	rel := l.rel(canonical)

	var out []Finding
	if filepath.Base(canonical) != doc.Name {
		out = append(out, Finding{
			Rule:     RuleNameNormalization,
			Severity: SeverityWarning,
			Path:     doc.RelPath,
			AbsPath:  doc.Path,
			Fixable:  true,
			Repair:   &Repair{MoveTo: rel},
			Message: fmt.Sprintf("filename is not the grammar's rendering of its own facts; the canonical name is %q",
				filepath.Base(canonical)),
		})
	}
	if filepath.Dir(canonical) != filepath.Dir(doc.Path) {
		out = append(out, Finding{
			Rule:     RuleWrongFolder,
			Severity: SeverityWarning,
			Path:     doc.RelPath,
			AbsPath:  doc.Path,
			Fixable:  true,
			Repair:   &Repair{MoveTo: rel},
			Message: fmt.Sprintf("a %s belongs in %s for this owner and period, not %s",
				want.DocType, l.rel(filepath.Dir(canonical)), l.rel(filepath.Dir(doc.Path))),
		})
	}
	return out
}

// checkTags covers the vocabulary and lifecycle rules.
//
// Every tag rule is skipped when the filesystem cannot store extended
// attributes: there, no file has tags, and reporting "missing lifecycle tag" on
// every document in the vault would be noise about the filesystem rather than
// about the vault.
func (l *Linter) checkTags(doc *search.Document) []Finding {
	if doc.TagsUnsupported {
		return nil
	}
	var out []Finding
	for _, unknown := range l.vocab.Unknown(doc.Tags) {
		out = append(out, Finding{
			Rule:     RuleUnknownTag,
			Severity: SeverityWarning,
			Path:     doc.RelPath,
			AbsPath:  doc.Path,
			Message: fmt.Sprintf("Finder tag %q is not in this vault's vocabulary; add it to tags: in vault.yaml, or remove it from the file",
				unknown),
		})
	}
	if l.cfg.Lint.RequireLifecycleTag && len(l.vocab.Lifecycle(doc.Tags)) == 0 {
		f := Finding{
			Rule:     RuleMissingLifecycleTag,
			Severity: SeverityWarning,
			Path:     doc.RelPath,
			AbsPath:  doc.Path,
			Message:  "no lifecycle tag (lint.require_lifecycle_tag is set)",
		}
		if tag, ok := l.lifecycleFromSidecar(doc); ok {
			f.Fixable = true
			f.Repair = &Repair{AddTag: tag}
			f.Message += fmt.Sprintf("; the sidecar records lifecycle %q, so --fix can apply it", tag)
		} else {
			f.Message += "; nothing records which one applies, so --fix leaves it alone (use `kagaz tag`)"
		}
		out = append(out, f)
	}
	return out
}

// lifecycleFromSidecar returns the lifecycle tag a document's sidecar names, if
// it names exactly one and that one is in the vocabulary. Anything less certain
// is not a fix — a wrong lifecycle tag silently misrepresents which document is
// the current one.
func (l *Linter) lifecycleFromSidecar(doc *search.Document) (string, bool) {
	if doc.Meta == nil {
		return "", false
	}
	v, ok := doc.Meta.Field("lifecycle")
	if !ok {
		return "", false
	}
	candidates := tags.Normalize(strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	}))
	if len(candidates) != 1 {
		return "", false
	}
	if l.vocab.Kind(candidates[0]) != tags.KindLifecycle {
		return "", false
	}
	return candidates[0], true
}

// checkSidecar covers the stale-sidecar rule. An evicted document is skipped:
// its bytes are not on this machine, so its hash cannot be computed and a
// "stale" verdict would be a guess.
func (l *Linter) checkSidecar(doc *search.Document) []Finding {
	if doc.Meta == nil || doc.Evicted || doc.Meta.SourceSHA == "" {
		return nil
	}
	sum, err := move.SHA256(doc.Path)
	if err != nil {
		return nil
	}
	if !doc.Meta.Stale(sum) {
		return nil
	}
	return []Finding{{
		Rule:     RuleStaleSidecar,
		Severity: SeverityWarning,
		Path:     doc.RelPath,
		AbsPath:  doc.Path,
		Message: fmt.Sprintf("sidecar was extracted from different bytes (sidecar %s…, file %s…); re-extract with `kagaz ingest --reindex`",
			short(doc.Meta.SourceSHA), short(sum)),
	}}
}

// checkSingleActive enforces lint.single_active_per_doctype_per_person: for the
// listed doctypes, at most one document per person may be tagged `active`.
//
// Every document in an offending group is reported, because the rule cannot
// know which one is current — that is exactly why it is not auto-fixable.
// `kagaz supersede` is the command that resolves it.
func (l *Linter) checkSingleActive(tree *search.Tree) []Finding {
	if len(l.cfg.Lint.SingleActivePerDocTypePerPerson) == 0 {
		return nil
	}
	watched := map[string]bool{}
	for _, dt := range l.cfg.Lint.SingleActivePerDocTypePerPerson {
		watched[config.Slug(dt)] = true
	}

	type key struct{ doctype, owner string }
	groups := map[key][]*search.Document{}
	for i := range tree.Documents {
		doc := &tree.Documents[i]
		if doc.TagsUnsupported || !doc.HasTag("active") {
			continue
		}
		dt := doc.DocType()
		if !watched[dt] {
			continue
		}
		owners := doc.Owners()
		if len(owners) == 0 {
			owners = []string{""}
		}
		for _, o := range owners {
			k := key{doctype: dt, owner: config.Slug(o)}
			groups[k] = append(groups[k], doc)
		}
	}

	var out []Finding
	for k, docs := range groups {
		if len(docs) < 2 {
			continue
		}
		paths := make([]string, 0, len(docs))
		for _, d := range docs {
			paths = append(paths, d.RelPath)
		}
		sort.Strings(paths)
		who := k.owner
		if who == "" {
			who = "(no owner)"
		}
		for _, d := range docs {
			out = append(out, Finding{
				Rule:     RuleMultipleActive,
				Severity: SeverityError,
				Path:     d.RelPath,
				AbsPath:  d.Path,
				Message: fmt.Sprintf("%d documents are tagged active for doctype %q and %s (%s); mark the superseded ones with `kagaz supersede`",
					len(docs), k.doctype, who, strings.Join(paths, ", ")),
			})
		}
	}
	return out
}

// renameFromSidecar computes the canonical path for a document whose filename
// does not parse, using only facts recorded in its sidecar. It returns false
// unless every field the grammar requires is present and the doctype is in the
// catalog — a rename built on a guess is worse than leaving the file alone.
func (l *Linter) renameFromSidecar(doc *search.Document) (string, bool) {
	if doc.Meta == nil {
		return "", false
	}
	dt := config.Slug(doc.Meta.DocType)
	cat, ok := l.catalog.CategoryOf(dt)
	if !ok || dt == doctypes.Unclassified {
		return "", false
	}
	want := conventions.Doc{
		DocType:    dt,
		Category:   cat,
		Owners:     doc.Meta.Owners,
		Identifier: doc.Meta.Identifier,
		Year:       doc.Meta.Year,
		Ext:        filepath.Ext(doc.Name),
	}
	canonical, err := l.conv.Path(want)
	if err != nil {
		return "", false
	}
	// The rendered name must itself parse back, or the "fix" would produce a
	// second name-grammar finding on the next run.
	if _, ok := l.conv.Parse(filepath.Base(canonical)); !ok {
		return "", false
	}
	return l.rel(canonical), true
}

// rel renders an absolute vault path relative to the vault root.
func (l *Linter) rel(abs string) string {
	r, err := filepath.Rel(l.cfg.VaultRoot, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(r)
}

// passwordWords are the tokens that mean a filename is carrying a credential.
//
// The check is whole-token and keyword-based on purpose. An entropy heuristic
// fires on invoice numbers, policy numbers and document ids, and a false
// positive here tells a user to rename a perfectly good document. Substring
// matching is just as bad: "Passport-Office" contains "pass", and "Pinterest"
// contains "pin". Note also that a filename's own separators — `_` in the
// default grammar — are word characters to a regexp `\b`, which is why the
// filename is split explicitly rather than matched with word boundaries.
var passwordWords = map[string]bool{
	"pw": true, "pwd": true, "passwd": true, "password": true, "passwords": true,
	"passcode": true, "passphrase": true, "pin": true, "pins": true,
	"otp": true, "secret": true, "secrets": true,
}

// tokenSplit splits a filename stem into the words a human would read in it.
var tokenSplit = regexp.MustCompile(`[^A-Za-z0-9]+`)

// passwordToken reports the password-looking token in a filename, if any.
func passwordToken(name string) (string, bool) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	for _, tok := range tokenSplit.Split(stem, -1) {
		if passwordWords[strings.ToLower(tok)] {
			return tok, true
		}
	}
	return "", false
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// errNoEngine is returned when a fix is requested without a usable move engine.
var errNoEngine = errors.New("lint: no move engine configured")
