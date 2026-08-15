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

// Roots returns the absolute directories a walk covers. The walk starts at the
// vault root: a document that is not in a category folder is still a document,
// and a tool whose job is keeping the vault honest cannot make the files it
// disapproves of invisible. Category folders are recognised during the walk,
// and anything outside them is reported (as the lint rule `unfiled`), not
// hidden.
func (s *Searcher) Roots() []string { return []string{filepath.Clean(s.cfg.VaultRoot)} }

// rootPlumbing lists the vault-root files that are Kagaz's own or the vault's
// documentation about itself, rather than filed documents. The check applies at
// the vault root only: a README inside a category folder is a document like any
// other.
func (s *Searcher) rootPlumbing(name string) bool {
	switch name {
	case config.FileName, "INDEX.md", "AGENTS.md", "README.md":
		return true
	}
	return name == filepath.Base(s.cfg.AuditLogPath())
}

// Scan walks the vault and returns every document, fully hydrated.
func (s *Searcher) Scan(ctx context.Context) (*Tree, error) {
	return s.scan(ctx, nil)
}

// Find returns the documents matching q, sorted by path relative to the vault
// root.
//
// Spotlight, when configured, is consulted first — but only to warm the
// sidecars of the files it believes match, never to decide which files are
// considered. The walk below sees every document either way.
func (s *Searcher) Find(ctx context.Context, q Query) ([]Document, error) {
	period, err := s.period(q)
	if err != nil {
		return nil, err
	}
	cache := s.prefetch(ctx, q)
	tree, err := s.scan(ctx, cache)
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

// prefetch asks Spotlight which files it believes match and reads their
// sidecars up front, returning them keyed by document path.
//
// This is the accelerator's entire effect, and it is deliberately one that
// cannot change an answer: the candidate set decides only what is read early,
// so a stale, partial or over-eager Spotlight index costs at most a little
// wasted or unhelpful I/O. Nothing is filtered by it.
func (s *Searcher) prefetch(ctx context.Context, q Query) map[string]*sidecar.Meta {
	if s.Spotlight == nil || !accelerable(q) {
		return nil
	}
	paths, ok, err := s.Spotlight.Narrow(ctx, s.cfg.VaultRoot, q)
	if err != nil || !ok || len(paths) == 0 {
		return nil
	}
	cache := make(map[string]*sidecar.Meta, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, seen := cache[abs]; seen {
			continue
		}
		meta, err := sidecar.ReadFile(sidecar.Path(abs))
		if err != nil {
			continue
		}
		cache[abs] = meta
	}
	return cache
}

// accelerable reports whether Spotlight can contribute anything to q. Only the
// content terms — free text and tags — are indexed by Spotlight in a form Kagaz
// can use; everything else is derived from the filename, which the walk reads
// for free.
func accelerable(q Query) bool {
	return q.Text != "" || len(q.Tags) > 0 || q.Active
}

// scan walks the vault. cache, when non-nil, holds sidecars already read by the
// Spotlight prefetch; it is an I/O shortcut and holds exactly what a read would
// have returned.
func (s *Searcher) scan(ctx context.Context, cache map[string]*sidecar.Meta) (*Tree, error) {
	tree := &Tree{}
	// Documents found as iCloud placeholders, keyed by their real path.
	evicted := map[string]bool{}
	// Sidecars seen, keyed by the document they claim to describe.
	sidecars := map[string]string{}
	docPaths := map[string]bool{}

	for _, root := range s.Roots() {
		if _, err := os.Stat(root); err != nil {
			continue // a vault whose root has not been created yet is not an error
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
				if path != root && s.skipDir(path, name) {
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
			case filepath.Dir(path) == root && s.rootPlumbing(name):
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
			doc := s.hydrate(path, s.categoryOf(path), size, mod)
			s.attachSidecar(&doc, cache)
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
		s.attachSidecar(&doc, cache)
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
// content. The manifest and staging directories are matched by their configured
// paths — a vault that renames its staging folder must still have it skipped —
// and by name as well, because the same folders nested inside a category are
// the same plumbing wherever a user put them.
func (s *Searcher) skipDir(path, name string) bool {
	if name == ".git" || strings.HasPrefix(name, ".") {
		return true
	}
	if path == s.cfg.ManifestDir() || path == s.cfg.StagingDir() {
		return true
	}
	return name == filepath.Base(s.cfg.ManifestDir()) || name == filepath.Base(s.cfg.StagingDir())
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

// attachSidecar reads the document's sidecar, preferring one already read by
// the Spotlight prefetch. A malformed sidecar is ignored rather than fatal.
func (s *Searcher) attachSidecar(doc *Document, cache map[string]*sidecar.Meta) {
	if meta, ok := cache[doc.Path]; ok {
		if meta != nil {
			doc.Meta = meta
			doc.HasSidecar = true
		}
		return
	}
	meta, err := sidecar.ReadFile(doc.SidecarPath)
	if err != nil || meta == nil {
		return
	}
	doc.Meta = meta
	doc.HasSidecar = true
}

// categoryOf returns the category whose folder contains path, preferring the
// most specific when one category's folder is nested inside another's. An empty
// result means the file is outside every configured category, which lint
// reports as `unfiled`.
func (s *Searcher) categoryOf(path string) string {
	best, bestLen := "", -1
	for name, cat := range s.cfg.Structure {
		root := filepath.Clean(filepath.Join(s.cfg.VaultRoot, cat.Path))
		if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
			continue
		}
		if len(root) > bestLen || (len(root) == bestLen && name < best) {
			best, bestLen = name, len(root)
		}
	}
	return best
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
