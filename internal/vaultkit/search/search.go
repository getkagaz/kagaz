// Package search is the read-only query layer over a vault: it walks the tree,
// reads each document's facts (parsed filename, Finder tags, sidecar) and
// applies the filters behind `kagaz find`.
//
// Three properties matter more than speed here:
//
//   - The filesystem is the only source of truth. Spotlight (see spotlight.go)
//     narrows the candidate set when it is available, but every candidate is
//     verified against the walk, and Kagaz never reports a document Spotlight
//     merely believes in.
//   - Missing optional tooling is never an error. A filesystem without extended
//     attributes yields documents with no tags rather than a failed query, so a
//     tag filter simply matches nothing.
//   - Sidecars are metadata, never results. A `.<name>.meta.yaml` dotfile is
//     attached to its document and is never returned in its own right.
package search

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/conventions"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/fycal"
	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// Document is one file in the vault together with everything Kagaz knows about
// it without opening it: the facts encoded in its name, its Finder tags and its
// sidecar.
type Document struct {
	// Path is the absolute path of the document.
	Path string
	// RelPath is the path relative to the vault root, always slash-separated.
	// It is the stable identity used for sorting and for generated output,
	// because an absolute path is machine-specific.
	RelPath string
	// Name is the base filename.
	Name string
	// Category is the vault category whose folder the file was found under.
	Category string
	// Doc holds the facts parsed out of the filename. Only meaningful when
	// Parsed is true.
	Doc conventions.Doc
	// Parsed reports whether the filename matched the vault's grammar.
	Parsed bool
	// Tags are the document's Finder tags, lowercased and sorted. Always empty
	// on a filesystem without extended-attribute support.
	Tags []string
	// TagsUnsupported records that the tags could not be read because the
	// filesystem does not support extended attributes, as opposed to the
	// document genuinely having none.
	TagsUnsupported bool
	// Meta is the parsed sidecar, or nil when the document has none.
	Meta *sidecar.Meta
	// SidecarPath is where the sidecar lives (whether or not it exists).
	SidecarPath string
	// HasSidecar reports whether a sidecar file exists for this document.
	HasSidecar bool
	// Evicted reports that the file is an iCloud placeholder: its bytes are not
	// on this machine until Materialize is called.
	Evicted bool
	// Size and ModTime come from the directory entry; both are zero for an
	// evicted document.
	Size    int64
	ModTime time.Time
}

// DocType is the document's best-known doctype: the one parsed from the
// filename, falling back to the sidecar's.
func (d *Document) DocType() string {
	if d.Parsed && d.Doc.DocType != "" {
		return d.Doc.DocType
	}
	if d.Meta != nil {
		return config.Slug(d.Meta.DocType)
	}
	return ""
}

// Owners returns the document's owner display names: those parsed from the
// filename, falling back to the sidecar's.
func (d *Document) Owners() []string {
	if d.Parsed && len(d.Doc.Owners) > 0 {
		return d.Doc.Owners
	}
	if d.Meta != nil {
		return d.Meta.Owners
	}
	return nil
}

// Year returns the document's year: the one parsed from the filename, falling
// back to the sidecar's. Zero means unknown.
func (d *Document) Year() int {
	if d.Parsed && d.Doc.Year != 0 {
		return d.Doc.Year
	}
	if d.Meta != nil {
		return d.Meta.Year
	}
	return 0
}

// HasTag reports whether the document carries tag.
func (d *Document) HasTag(tag string) bool {
	want := strings.ToLower(strings.TrimSpace(tag))
	for _, t := range d.Tags {
		if t == want {
			return true
		}
	}
	return false
}

// Query is a set of filters. The zero Query matches every document.
type Query struct {
	// Text is a loose full-text term matched against the filename, the path,
	// the parsed facts, the sidecar's extracted text and its extracted fields.
	Text string
	// Person is a person's display name or tag, as written in vault.yaml.
	Person string
	// Company is a company tag from the vocabulary. It also matches a document
	// whose Identifier slugifies to the same value, so it works on a filesystem
	// with no tag support.
	Company string
	// Area is an area tag from the vocabulary. Areas have no filename
	// representation, so this filter matches on tags only.
	Area string
	// DocType is a catalog doctype name.
	DocType string
	// Tags must all be present on a document for it to match.
	Tags []string
	// Active restricts results to documents tagged `active`.
	Active bool
	// Period is a calendar or fiscal period expression understood by
	// fycal.Calendar.ParsePeriod, e.g. "2026", "FY2026", "FY2026Q3".
	Period string
}

// Empty reports whether the query has no filters at all.
func (q Query) Empty() bool {
	return q.Text == "" && q.Person == "" && q.Company == "" && q.Area == "" &&
		q.DocType == "" && len(q.Tags) == 0 && !q.Active && q.Period == ""
}

// Tree is the result of a full walk.
type Tree struct {
	// Documents are every document found, sorted by RelPath.
	Documents []Document
	// OrphanSidecars are sidecar files whose document does not exist, sorted by
	// path relative to the vault root.
	OrphanSidecars []string
	// Warnings records degraded behaviour (an unreadable directory, absent
	// extended-attribute support) without failing the query.
	Warnings []string
}

// Searcher answers queries for one vault.
type Searcher struct {
	// Spotlight, when non-nil, narrows the candidate set before the filesystem
	// walk. Leaving it nil forces the pure-filesystem path, which is exactly
	// what tests do — there is no environment variable involved.
	Spotlight Finder
	// ReadTags reads a file's Finder tags. It defaults to tags.Read and exists
	// so tests can supply tags on a filesystem that cannot store them.
	ReadTags func(path string) ([]string, error)

	cfg     *config.Config
	conv    *conventions.Conventions
	catalog *doctypes.Catalog
	cal     fycal.Calendar
}

// New builds a Searcher for a vault. Spotlight acceleration is off by default;
// set the Spotlight field (to NewMDFind(), say) to enable it.
func New(cfg *config.Config) (*Searcher, error) {
	conv, err := conventions.New(cfg)
	if err != nil {
		return nil, err
	}
	cat, err := doctypes.Resolve(cfg)
	if err != nil {
		return nil, err
	}
	return &Searcher{
		cfg:     cfg,
		conv:    conv,
		catalog: cat,
		cal:     fycal.New(cfg.FiscalYear.StartMonth, cfg.FiscalYear.LabelFormat),
		ReadTags: func(path string) ([]string, error) {
			return tags.Read(path)
		},
	}, nil
}

// Config returns the vault configuration the Searcher was built from.
func (s *Searcher) Config() *config.Config { return s.cfg }

// Conventions returns the compiled naming conventions.
func (s *Searcher) Conventions() *conventions.Conventions { return s.conv }

// Catalog returns the resolved doctype catalog.
func (s *Searcher) Catalog() *doctypes.Catalog { return s.catalog }

// Calendar returns the vault's fiscal calendar.
func (s *Searcher) Calendar() fycal.Calendar { return s.cal }

// Roots returns the absolute directories a walk covers: the folder of every
// configured category, sorted, with any directory nested inside another dropped
// so no file is visited twice.
//
// Documents live in category folders. The vault root itself holds vault.yaml,
// the generated INDEX.md/AGENTS.md, the manifests folder and the staging
// folder, none of which is a document, so the root is deliberately not walked
// as a whole.
func (s *Searcher) Roots() []string {
	seen := map[string]bool{}
	var roots []string
	for _, cat := range s.cfg.Structure {
		p := filepath.Clean(filepath.Join(s.cfg.VaultRoot, cat.Path))
		if seen[p] {
			continue
		}
		seen[p] = true
		roots = append(roots, p)
	}
	sort.Strings(roots)

	var kept []string
	for _, r := range roots {
		nested := false
		for _, other := range kept {
			if r != other && strings.HasPrefix(r, other+string(filepath.Separator)) {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, r)
		}
	}
	return kept
}

// categoryOfRoot maps an absolute category root back to its category name.
func (s *Searcher) categoryOfRoot(root string) string {
	for name, cat := range s.cfg.Structure {
		if filepath.Clean(filepath.Join(s.cfg.VaultRoot, cat.Path)) == root {
			return name
		}
	}
	return ""
}

// Scan walks the vault and returns every document, fully hydrated.
func (s *Searcher) Scan(ctx context.Context) (*Tree, error) {
	return s.scan(ctx, nil, Query{})
}

// Find returns the documents matching q, sorted by path relative to the vault
// root.
func (s *Searcher) Find(ctx context.Context, q Query) ([]Document, error) {
	period, err := s.period(q)
	if err != nil {
		return nil, err
	}
	candidates := s.narrow(ctx, q)
	tree, err := s.scan(ctx, candidates, q)
	if err != nil {
		return nil, err
	}
	out := make([]Document, 0, len(tree.Documents))
	for i := range tree.Documents {
		if s.match(&tree.Documents[i], q, period) {
			out = append(out, tree.Documents[i])
		}
	}
	return out, nil
}

// narrow asks Spotlight for a candidate set. A nil result means "no narrowing":
// either Spotlight is unavailable, the query is not accelerable, or Spotlight
// answered with nothing at all — which is indistinguishable from an index that
// has not caught up yet, and so is treated as no answer rather than as an empty
// result set.
func (s *Searcher) narrow(ctx context.Context, q Query) map[string]bool {
	if s.Spotlight == nil || !accelerable(q) {
		return nil
	}
	paths, ok, err := s.Spotlight.Narrow(ctx, s.cfg.VaultRoot, q)
	if err != nil || !ok || len(paths) == 0 {
		return nil
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		if abs, err := filepath.Abs(p); err == nil {
			set[abs] = true
		}
	}
	return set
}

// accelerable reports whether Spotlight can contribute anything to q. Only the
// content terms — free text and tags — are indexed by Spotlight in a form Kagaz
// can use; everything else is derived from the filename, which the walk reads
// for free.
func accelerable(q Query) bool {
	return q.Text != "" || len(q.Tags) > 0 || q.Active
}

// scan walks the category roots. candidates, when non-nil, is Spotlight's
// narrowed set: a file outside it that does not already match on the facts in
// its own name is skipped without reading its sidecar. That skip is the entire
// benefit of the accelerator, and it is also its entire risk — see the note on
// Finder in spotlight.go.
func (s *Searcher) scan(ctx context.Context, candidates map[string]bool, q Query) (*Tree, error) {
	tree := &Tree{}
	// Documents found as iCloud placeholders, keyed by their real path.
	evicted := map[string]bool{}
	// Sidecars seen, keyed by the document they claim to describe.
	sidecars := map[string]string{}
	docPaths := map[string]bool{}

	period, err := s.period(q)
	if err != nil {
		return nil, err
	}

	for _, root := range s.Roots() {
		category := s.categoryOfRoot(root)
		if _, err := os.Stat(root); err != nil {
			continue // a category folder that has not been created yet is not an error
		}
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if err != nil {
				tree.Warnings = append(tree.Warnings, path+": "+err.Error())
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && (skipDir(name) || path == s.cfg.ManifestDir() || path == s.cfg.StagingDir()) {
					return fs.SkipDir
				}
				return nil
			}
			switch {
			case sidecar.IsSidecar(path):
				sidecars[sidecar.DocumentFor(path)] = path
				return nil
			case isPlaceholder(name):
				doc := documentForPlaceholder(path)
				evicted[doc] = true
				return nil
			case strings.HasPrefix(name, "."):
				// Any other dotfile is Kagaz's business or the operating
				// system's, never a document.
				return nil
			}
			docPaths[path] = true
			info, ierr := d.Info()
			var size int64
			var mod time.Time
			if ierr == nil {
				size = info.Size()
				mod = info.ModTime()
			}
			doc := s.hydrate(path, category, size, mod)
			if candidates != nil && !candidates[path] && !s.match(&doc, q, period) {
				// Spotlight excluded this file and nothing in its name, path or
				// tags matches either: skip the sidecar read.
				return nil
			}
			s.attachSidecar(&doc)
			tree.Documents = append(tree.Documents, doc)
			return nil
		})
		if walkErr != nil {
			if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
				return nil, walkErr
			}
			tree.Warnings = append(tree.Warnings, root+": "+walkErr.Error())
		}
	}

	// An evicted document has no directory entry of its own; the placeholder is
	// all there is, so synthesize the document from it.
	for path := range evicted {
		if docPaths[path] {
			continue
		}
		doc := s.hydrate(path, s.categoryOf(path), 0, time.Time{})
		doc.Evicted = true
		s.attachSidecar(&doc)
		tree.Documents = append(tree.Documents, doc)
		docPaths[path] = true
	}

	for docPath, sidePath := range sidecars {
		if !docPaths[docPath] {
			tree.OrphanSidecars = append(tree.OrphanSidecars, s.rel(sidePath))
		}
	}

	sort.Slice(tree.Documents, func(i, j int) bool {
		return tree.Documents[i].RelPath < tree.Documents[j].RelPath
	})
	sort.Strings(tree.OrphanSidecars)
	sort.Strings(tree.Warnings)
	return tree, nil
}

// skipDir reports whether a directory is Kagaz plumbing rather than vault
// content, by name. The vault's configured manifest and staging directories are
// checked by absolute path at the call site; these names are also skipped
// wherever they appear, because a manifests folder or a staging folder nested
// inside a category is the same plumbing wherever a user put it.
func skipDir(name string) bool {
	if name == ".git" {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	return name == "manifests" || name == "_To-Delete-After-Verification"
}

// isPlaceholder reports whether name is an iCloud eviction placeholder.
func isPlaceholder(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, icloudSuffix)
}

// hydrate builds a Document from a path without reading its sidecar.
func (s *Searcher) hydrate(path, category string, size int64, mod time.Time) Document {
	name := filepath.Base(path)
	doc := Document{
		Path:        path,
		RelPath:     s.rel(path),
		Name:        name,
		Category:    category,
		SidecarPath: sidecar.Path(path),
		Size:        size,
		ModTime:     mod,
	}
	if parsed, ok := s.conv.Parse(name); ok {
		doc.Doc = parsed
		doc.Doc.Owners = s.ResolveOwners(parsed.Owners)
		doc.Parsed = true
		if cat, ok := s.catalog.CategoryOf(parsed.DocType); ok {
			doc.Doc.Category = cat
		}
	}
	if s.ReadTags != nil {
		list, err := s.ReadTags(path)
		switch {
		case err == nil:
			doc.Tags = tags.Normalize(list)
		case errors.Is(err, tags.ErrUnsupported):
			doc.TagsUnsupported = true
		}
		// Any other error (a vanished file, a permission problem) leaves the
		// document with no tags: a query must never fail because one file's
		// metadata could not be read.
	}
	if !doc.Evicted && IsEvicted(path) {
		doc.Evicted = true
	}
	return doc
}

// ResolveOwners maps the owner words parsed out of a filename back onto the
// vault's configured people.
//
// It exists because a vault may — and the default grammar does — use the same
// character for owner_groups.separator_filename and filename.word_separator.
// "Alex-Rao" then parses into the two owner words "Alex" and "Rao", which is
// all conventions.Parse can know without the people list. Reading that as two
// owners would put the document in a shared folder and make lint demand a move
// of a correctly filed file, so the people list is consulted here: a run of
// words that spells a configured person becomes that person.
//
// A token that does not resolve is returned unchanged. Kagaz never invents an
// owner, and a document belonging to someone not in vault.yaml stays exactly as
// its filename says.
func (s *Searcher) ResolveOwners(parsed []string) []string {
	if len(parsed) == 0 || len(s.cfg.People) == 0 {
		return parsed
	}
	sep := s.cfg.OwnerGroup.SeparatorFilename
	if sep == "" {
		return parsed
	}
	words := make([]string, 0, len(parsed))
	for _, o := range parsed {
		words = append(words, s.conv.Word(o))
	}
	token := strings.Join(words, sep)

	// Longest spelling first, so "Alex-Rao" wins over a hypothetical "Alex".
	type person struct{ word, name string }
	people := make([]person, 0, len(s.cfg.People))
	for _, p := range s.cfg.People {
		if w := s.conv.Word(p.Name); w != "" {
			people = append(people, person{word: w, name: p.Name})
		}
	}
	sort.Slice(people, func(i, j int) bool {
		if len(people[i].word) != len(people[j].word) {
			return len(people[i].word) > len(people[j].word)
		}
		return people[i].word < people[j].word
	})

	var out []string
	for rest := token; rest != ""; {
		matched := false
		for _, p := range people {
			if !strings.HasPrefix(strings.ToLower(rest), strings.ToLower(p.word)) {
				continue
			}
			tail := rest[len(p.word):]
			if tail != "" && !strings.HasPrefix(tail, sep) {
				continue // a longer word that merely starts with this person's
			}
			out = append(out, p.name)
			rest = strings.TrimPrefix(tail, sep)
			matched = true
			break
		}
		if !matched {
			return parsed
		}
	}
	if len(out) == 0 {
		return parsed
	}
	return out
}

// attachSidecar reads the document's sidecar, if any. A malformed sidecar is
// ignored rather than fatal.
func (s *Searcher) attachSidecar(doc *Document) {
	if doc.Meta != nil || doc.HasSidecar {
		return
	}
	meta, err := sidecar.ReadFile(doc.SidecarPath)
	if err != nil || meta == nil {
		return
	}
	doc.Meta = meta
	doc.HasSidecar = true
}

// categoryOf returns the category whose folder contains path.
func (s *Searcher) categoryOf(path string) string {
	for name, cat := range s.cfg.Structure {
		root := filepath.Clean(filepath.Join(s.cfg.VaultRoot, cat.Path))
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return name
		}
	}
	return ""
}

// rel returns path relative to the vault root, slash-separated.
func (s *Searcher) rel(path string) string {
	r, err := filepath.Rel(s.cfg.VaultRoot, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

// period resolves q.Period once per query.
func (s *Searcher) period(q Query) (*fycal.Period, error) {
	if q.Period == "" {
		return nil, nil
	}
	p, err := s.cal.ParsePeriod(q.Period)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// match applies every filter in q to doc.
func (s *Searcher) match(doc *Document, q Query, period *fycal.Period) bool {
	if q.Active && !doc.HasTag("active") {
		return false
	}
	for _, t := range q.Tags {
		if !doc.HasTag(t) {
			return false
		}
	}
	if q.DocType != "" && doc.DocType() != config.Slug(q.DocType) {
		return false
	}
	if q.Person != "" && !s.matchPerson(doc, q.Person) {
		return false
	}
	if q.Company != "" && !matchCompany(doc, q.Company) {
		return false
	}
	if q.Area != "" && !doc.HasTag(config.Slug(q.Area)) {
		return false
	}
	if period != nil && !s.matchPeriod(doc, *period) {
		return false
	}
	if q.Text != "" && !s.matchText(doc, q.Text) {
		return false
	}
	return true
}

// matchPerson matches a person by display name or tag, against the parsed
// owners, the Finder tags and the owner folder in the path — in that order, so
// the filter still works when tags are unavailable.
func (s *Searcher) matchPerson(doc *Document, who string) bool {
	name, tag := who, config.Slug(who)
	if p, ok := s.cfg.Person(who); ok {
		name, tag = p.Name, p.Tag
	}
	for _, o := range doc.Owners() {
		if config.Slug(o) == config.Slug(name) {
			return true
		}
	}
	if doc.HasTag(tag) {
		return true
	}
	folder := s.conv.Word(name)
	if folder == "" {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(doc.RelPath), "/") {
		if strings.EqualFold(seg, folder) {
			return true
		}
	}
	return false
}

// matchCompany matches a company tag, falling back to the identifier field of
// the filename so the filter works without extended attributes.
func matchCompany(doc *Document, company string) bool {
	want := config.Slug(company)
	if want == "" {
		return false
	}
	if doc.HasTag(want) {
		return true
	}
	if doc.Parsed && config.Slug(doc.Doc.Identifier) == want {
		return true
	}
	if doc.Meta != nil && config.Slug(doc.Meta.Identifier) == want {
		return true
	}
	return false
}

// matchPeriod tests a document's year against a resolved period. A document
// whose year is unknown never matches a period filter: guessing from the file's
// modification time would make results depend on when the vault was last
// copied.
func (s *Searcher) matchPeriod(doc *Document, p fycal.Period) bool {
	year := doc.Year()
	if year == 0 {
		return false
	}
	from, to := s.cal.YearStarting(year).Range()
	// Overlap of the two half-open intervals.
	return from.Before(p.To) && p.From.Before(to)
}

// matchText applies the loose full-text term.
func (s *Searcher) matchText(doc *Document, text string) bool {
	needle := strings.ToLower(strings.TrimSpace(text))
	if needle == "" {
		return true
	}
	haystacks := []string{strings.ToLower(doc.Name), strings.ToLower(doc.RelPath)}
	if doc.Parsed {
		haystacks = append(haystacks,
			strings.ToLower(doc.Doc.DocType),
			strings.ToLower(doc.Doc.Identifier),
			strings.ToLower(doc.Doc.Modifier),
			strings.ToLower(strings.Join(doc.Doc.Owners, " ")),
		)
	}
	haystacks = append(haystacks, strings.Join(doc.Tags, " "))
	if doc.Meta != nil {
		haystacks = append(haystacks,
			strings.ToLower(doc.Meta.Text),
			strings.ToLower(doc.Meta.Identifier),
			strings.ToLower(doc.Meta.DocType),
			strings.ToLower(strings.Join(doc.Meta.Owners, " ")),
		)
		for _, v := range doc.Meta.Fields {
			haystacks = append(haystacks, strings.ToLower(v))
		}
	}
	for _, h := range haystacks {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
