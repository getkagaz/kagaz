package search

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// fixtureVault is the committed, hand-inspectable vault every golden assertion
// in this package runs against. See testdata/fixture-vault/README.md for what
// each file is deliberately there to exercise.
const fixtureVault = "../../../testdata/fixture-vault/vault.yaml"

func loadFixture(t *testing.T) *config.Config {
	t.Helper()
	abs, err := filepath.Abs(fixtureVault)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(abs)
	if err != nil {
		t.Fatalf("load fixture vault: %v", err)
	}
	return cfg
}

func newFixtureSearcher(t *testing.T) *Searcher {
	t.Helper()
	s, err := New(loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The five documents the fixture vault contains, in RelPath order.
var fixtureDocs = []string{
	"Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt",
	"Financial/Alex-Rao/FY 2026/old invoice notes.txt",
	"Financial/Sam-Rao/FY 2025/Receipt_Sam-Rao_Globex_2025.txt",
	"Identity/Alex-Rao/Passport_Alex-Rao_Passport-Office_2024.txt",
	"Travel/Sam-Rao/Boarding-Pass_Sam-Rao_United-Airlines.txt",
}

func relPaths(docs []Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.RelPath)
	}
	return out
}

func TestScanFixture(t *testing.T) {
	s := newFixtureSearcher(t)
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := relPaths(tree.Documents); !reflect.DeepEqual(got, fixtureDocs) {
		t.Errorf("documents:\n got %q\nwant %q", got, fixtureDocs)
	}
	want := []string{"Travel/Sam-Rao/.Ticket_Sam-Rao_Delta-Airlines.txt.meta.yaml"}
	if !reflect.DeepEqual(tree.OrphanSidecars, want) {
		t.Errorf("orphan sidecars: got %q want %q", tree.OrphanSidecars, want)
	}
	for _, d := range tree.Documents {
		if strings.HasPrefix(filepath.Base(d.Path), ".") {
			t.Errorf("a dotfile was returned as a document: %s", d.RelPath)
		}
	}
}

func TestScanParsesFactsAndSidecars(t *testing.T) {
	s := newFixtureSearcher(t)
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Document{}
	for _, d := range tree.Documents {
		byPath[d.RelPath] = d
	}

	invoice := byPath["Financial/Alex-Rao/FY 2026/Invoice_Alex-Rao_Acme-Corp_2026.txt"]
	if !invoice.Parsed {
		t.Fatal("invoice filename did not parse")
	}
	if invoice.Doc.DocType != "invoice" || invoice.Doc.Category != "financial" {
		t.Errorf("invoice doctype/category: got %q/%q", invoice.Doc.DocType, invoice.Doc.Category)
	}
	if got := invoice.Doc.Owners; !reflect.DeepEqual(got, []string{"Alex Rao"}) {
		t.Errorf("invoice owners: got %q", got)
	}
	if invoice.Doc.Identifier != "Acme Corp" || invoice.Doc.Year != 2026 {
		t.Errorf("invoice identifier/year: got %q/%d", invoice.Doc.Identifier, invoice.Doc.Year)
	}
	if !invoice.HasSidecar || invoice.Meta == nil || invoice.Meta.Fields["invoice_number"] != "INV-2026-0417" {
		t.Errorf("invoice sidecar not attached: %+v", invoice.Meta)
	}

	stray := byPath["Financial/Alex-Rao/FY 2026/old invoice notes.txt"]
	if stray.Parsed {
		t.Error("the deliberately non-conforming filename parsed; the grammar is too lenient")
	}
	if stray.HasSidecar {
		t.Error("the non-conforming file should have no sidecar")
	}

	pass := byPath["Travel/Sam-Rao/Boarding-Pass_Sam-Rao_United-Airlines.txt"]
	if pass.Doc.DocType != "boarding-pass" || pass.Category != "travel" {
		t.Errorf("boarding pass facts: %q %q", pass.Doc.DocType, pass.Category)
	}
	if pass.HasSidecar {
		t.Error("the boarding pass deliberately has no sidecar")
	}
}

func TestFilterMatrix(t *testing.T) {
	s := newFixtureSearcher(t)
	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"no filters returns everything", Query{}, fixtureDocs},
		{"doctype", Query{DocType: "invoice"}, []string{fixtureDocs[0]}},
		{"doctype is slugified", Query{DocType: "Boarding-Pass"}, []string{fixtureDocs[4]}},
		{"doctype not in the vault", Query{DocType: "visa"}, nil},
		{"person by display name", Query{Person: "Alex Rao"}, []string{fixtureDocs[0], fixtureDocs[1], fixtureDocs[3]}},
		{"person by tag", Query{Person: "sam-rao"}, []string{fixtureDocs[2], fixtureDocs[4]}},
		{"person and doctype", Query{Person: "Alex Rao", DocType: "passport"}, []string{fixtureDocs[3]}},
		{"company from the identifier field", Query{Company: "acme-corp"}, []string{fixtureDocs[0]}},
		{"company spelled as a display name", Query{Company: "Globex"}, []string{fixtureDocs[2]}},
		{"area matches on tags only, and the fixture has none", Query{Area: "tax"}, nil},
		{"fiscal period", Query{Period: "FY2026"}, []string{fixtureDocs[0]}},
		{"calendar period", Query{Period: "2025"}, []string{fixtureDocs[2]}},
		{"period excludes documents with no year", Query{Period: "2024"}, []string{fixtureDocs[3]}},
		{"text matches a filename", Query{Text: "boarding"}, []string{fixtureDocs[4]}},
		{"text matches sidecar text", Query{Text: "INV-2026-0417"}, []string{fixtureDocs[0]}},
		{"text matches a path segment", Query{Text: "FY 2025"}, []string{fixtureDocs[2]}},
		{"text is case-insensitive", Query{Text: "gLoBeX"}, []string{fixtureDocs[2]}},
		{"text matching nothing", Query{Text: "zzzz-no-such-thing"}, nil},
		{"tag filter with no tags on disk", Query{Tags: []string{"active"}}, nil},
		{"active filter with no tags on disk", Query{Active: true}, nil},
		{"filters are ANDed", Query{Person: "Alex Rao", Period: "FY2026", Text: "acme"}, []string{fixtureDocs[0]}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Find(context.Background(), tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(relPaths(got), tc.want) {
				t.Errorf("got %q want %q", relPaths(got), tc.want)
			}
		})
	}
}

func TestFindRejectsUnparseablePeriod(t *testing.T) {
	s := newFixtureSearcher(t)
	if _, err := s.Find(context.Background(), Query{Period: "last tuesday"}); err == nil {
		t.Fatal("want an error for an unparseable period")
	}
}

// fakeSpotlight is a stand-in for mdfind. It never touches the filesystem: the
// candidate set is whatever the test hands it, which is how "Spotlight's index
// is stale/lying" is simulated without a Mac.
type fakeSpotlight struct {
	paths   []string
	ok      bool
	err     error
	queries []string
}

func (f *fakeSpotlight) Available() bool { return true }

func (f *fakeSpotlight) Narrow(_ context.Context, _ string, q Query) ([]string, bool, error) {
	f.queries = append(f.queries, MDFindExpr(q))
	return f.paths, f.ok, f.err
}

// TestSpotlightDoesNotChangeResults runs the same queries with and without the
// accelerator over the fixture vault. Forcing the non-Spotlight path is just a
// nil field — there is no environment variable to set and no build tag.
func TestSpotlightDoesNotChangeResults(t *testing.T) {
	plain := newFixtureSearcher(t)
	root := plain.Config().VaultRoot
	abs := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

	queries := []Query{
		{Text: "invoice"},
		{Text: "acme"},
		{Text: "INV-2026-0417"},
		{Text: "receipt", Person: "Sam Rao"},
		{Tags: []string{"active"}},
		{Active: true, Text: "passport"},
	}

	spotlights := map[string]*fakeSpotlight{
		// A complete, current index.
		"complete": {ok: true, paths: []string{
			abs(fixtureDocs[0]), abs(fixtureDocs[1]), abs(fixtureDocs[2]),
			abs(fixtureDocs[3]), abs(fixtureDocs[4]),
		}},
		// An index that over-reports: every candidate must still be verified
		// against the filesystem, so the extra file must not appear.
		"over-reporting": {ok: true, paths: []string{
			abs(fixtureDocs[0]), abs(fixtureDocs[1]), abs(fixtureDocs[2]),
			abs(fixtureDocs[3]), abs(fixtureDocs[4]),
			abs("Financial/Alex-Rao/FY 2026/not-a-real-file.txt"),
		}},
		// The ordinary stale-index case: Spotlight names some of the vault but
		// not the file that actually matches. This is the case that used to
		// lose a result, and it is why the candidate set no longer decides
		// which files are considered.
		"under-reporting": {ok: true, paths: []string{
			abs(fixtureDocs[4]),
		}},
		// An index that knows only files which match nothing in these queries.
		"under-reporting hard": {ok: true, paths: []string{
			abs(fixtureDocs[3]),
		}},
		// Spotlight present but answering nothing: indistinguishable from an
		// index that has not caught up, so Kagaz must fall back to the walk.
		"empty answer": {ok: true},
		// Spotlight declining to answer.
		"declined": {ok: false},
		// Spotlight failing outright.
		"error": {err: ErrNoMDFind},
	}

	for name, spot := range spotlights {
		t.Run(name, func(t *testing.T) {
			accelerated := newFixtureSearcher(t)
			accelerated.Spotlight = spot
			for _, q := range queries {
				want, err := plain.Find(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				got, err := accelerated.Find(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(relPaths(got), relPaths(want)) {
					t.Errorf("query %+v: with Spotlight %q, without %q", q, relPaths(got), relPaths(want))
				}
			}
		})
	}
}

func TestSpotlightIsNotConsultedWithoutContentTerms(t *testing.T) {
	s := newFixtureSearcher(t)
	spot := &fakeSpotlight{ok: true}
	s.Spotlight = spot
	if _, err := s.Find(context.Background(), Query{Person: "Alex Rao"}); err != nil {
		t.Fatal(err)
	}
	if len(spot.queries) != 0 {
		t.Errorf("Spotlight was consulted for a filename-derived filter: %q", spot.queries)
	}
	if _, err := s.Find(context.Background(), Query{Text: "acme"}); err != nil {
		t.Fatal(err)
	}
	if len(spot.queries) != 1 {
		t.Errorf("Spotlight should have been consulted once for a text query, got %q", spot.queries)
	}
}

func TestMDFindExpr(t *testing.T) {
	cases := []struct {
		name string
		q    Query
		want string
	}{
		{"no content terms", Query{Person: "Alex Rao"}, ""},
		{"one tag", Query{Tags: []string{"active"}}, `kMDItemUserTags == "active"c`},
		{"active flag", Query{Active: true}, `kMDItemUserTags == "active"c`},
		{"tags are ANDed", Query{Tags: []string{"tax", "fy2026"}},
			`kMDItemUserTags == "tax"c && kMDItemUserTags == "fy2026"c`},
		{"duplicate tags collapse", Query{Tags: []string{"active"}, Active: true},
			`kMDItemUserTags == "active"c`},
		{"text", Query{Text: "acme"},
			`(kMDItemTextContent == "*acme*"cd || kMDItemDisplayName == "*acme*"cd)`},
		{"tag and text", Query{Tags: []string{"tax"}, Text: "acme"},
			`kMDItemUserTags == "tax"c && (kMDItemTextContent == "*acme*"cd || kMDItemDisplayName == "*acme*"cd)`},
		{"quotes cannot escape the expression", Query{Text: `a" || kMDItemFSName == "*`},
			`(kMDItemTextContent == "*a || kMDItemFSName == **"cd || kMDItemDisplayName == "*a || kMDItemFSName == **"cd)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MDFindExpr(tc.q); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestMDFindUnavailableIsNotAnError(t *testing.T) {
	m := &MDFind{Bin: ""}
	// Point at a binary that cannot exist so the lookup fails deterministically
	// on any machine, with or without Spotlight.
	m.Bin = ""
	if _, ok, err := (&MDFind{Bin: filepath.Join(t.TempDir(), "definitely-not-mdfind")}).
		Narrow(context.Background(), t.TempDir(), Query{Text: "x"}); ok || err == nil {
		t.Fatalf("want a clear unavailable answer, got ok=%v err=%v", ok, err)
	}
}

// tagged builds a temp vault copy so tag-dependent behaviour can be tested with
// injected tags: extended attributes cannot be committed to git, so the fixture
// carries none and tests supply them here.
func taggedSearcher(t *testing.T, byPath map[string][]string) *Searcher {
	t.Helper()
	s := newFixtureSearcher(t)
	s.ReadTags = func(path string) ([]string, error) {
		rel, err := filepath.Rel(s.Config().VaultRoot, path)
		if err != nil {
			return nil, nil
		}
		return byPath[filepath.ToSlash(rel)], nil
	}
	return s
}

func TestTagFilters(t *testing.T) {
	s := taggedSearcher(t, map[string][]string{
		fixtureDocs[0]: {"active", "acme-corp", "fy2026"},
		fixtureDocs[3]: {"active", "confidential"},
	})
	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"single tag", Query{Tags: []string{"acme-corp"}}, []string{fixtureDocs[0]}},
		{"active", Query{Active: true}, []string{fixtureDocs[0], fixtureDocs[3]}},
		{"tags are ANDed", Query{Tags: []string{"active", "confidential"}}, []string{fixtureDocs[3]}},
		{"tag plus doctype", Query{Tags: []string{"active"}, DocType: "passport"}, []string{fixtureDocs[3]}},
		{"unknown tag matches nothing", Query{Tags: []string{"nope"}}, nil},
		{"company falls back to the tag", Query{Company: "acme-corp"}, []string{fixtureDocs[0]}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Find(context.Background(), tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(relPaths(got), tc.want) {
				t.Errorf("got %q want %q", relPaths(got), tc.want)
			}
		})
	}
}

func TestUnsupportedTagsDegradeToNoTags(t *testing.T) {
	s := newFixtureSearcher(t)
	s.ReadTags = func(string) ([]string, error) { return nil, tags.ErrUnsupported }
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatalf("a filesystem without xattr support must not fail a scan: %v", err)
	}
	if len(tree.Documents) != len(fixtureDocs) {
		t.Fatalf("got %d documents, want %d", len(tree.Documents), len(fixtureDocs))
	}
	for _, d := range tree.Documents {
		if !d.TagsUnsupported {
			t.Errorf("%s: TagsUnsupported not recorded", d.RelPath)
		}
		if len(d.Tags) != 0 {
			t.Errorf("%s: got tags %q", d.RelPath, d.Tags)
		}
	}
	got, err := s.Find(context.Background(), Query{Tags: []string{"active"}})
	if err != nil {
		t.Fatalf("a tag filter must return nothing, not fail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %q, want no results", relPaths(got))
	}
}

// tempVault writes a minimal vault.yaml and returns its loaded config.
func tempVault(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	yaml := "version: 1\nvault_root: .\npeople:\n  - name: Alex Rao\n" +
		"structure:\n  financial:\n    path: Financial\n    layout: \"{Owner}\"\n"
	path := filepath.Join(dir, "vault.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEvictedDocumentIsFoundAndFlagged(t *testing.T) {
	cfg := tempVault(t)
	dir := filepath.Join(cfg.VaultRoot, "Financial", "Alex-Rao")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(dir, "Invoice_Alex-Rao_Acme_2026.pdf")
	if err := os.WriteFile(PlaceholderPath(doc), []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Documents) != 1 {
		t.Fatalf("got %d documents, want the evicted one", len(tree.Documents))
	}
	got := tree.Documents[0]
	if got.Name != "Invoice_Alex-Rao_Acme_2026.pdf" {
		t.Errorf("the placeholder was returned instead of the document it stands for: %s", got.Name)
	}
	if !got.Evicted {
		t.Error("document not flagged as evicted")
	}
	if !got.Parsed {
		t.Error("an evicted document should still yield its filename facts")
	}
}

func TestScanSkipsPlumbing(t *testing.T) {
	cfg := tempVault(t)
	mk := func(rel, content string) string {
		p := filepath.Join(cfg.VaultRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("Financial/Alex-Rao/Invoice_Alex-Rao_Acme_2026.txt", "hello")
	mk("Financial/manifests/20260101-000000_ingest.csv", "current_path,original_path,sha256\n")
	mk("Financial/_To-Delete-After-Verification/20260101-000000/old.txt", "staged")
	mk("Financial/.git/config", "[core]")
	mk("Financial/.hidden-note.txt", "hidden")
	mk("INDEX.md", "# generated")
	mk("vault.log", "audit")

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Financial/Alex-Rao/Invoice_Alex-Rao_Acme_2026.txt"}
	if !reflect.DeepEqual(relPaths(tree.Documents), want) {
		t.Errorf("got %q want %q", relPaths(tree.Documents), want)
	}
}

func TestCancelledContextStopsScan(t *testing.T) {
	s := newFixtureSearcher(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Scan(ctx); err == nil {
		t.Fatal("want the cancellation to surface")
	}
}

// TestSidecarOnlyMatchSurvivesAnUnderReportingSpotlight is the regression test
// for the bug this round fixed: "INV-2026-0417" appears only inside the
// invoice's sidecar text, and a Spotlight that returns a non-empty but
// incomplete list must not be able to hide it.
func TestSidecarOnlyMatchSurvivesAnUnderReportingSpotlight(t *testing.T) {
	s := newFixtureSearcher(t)
	root := s.Config().VaultRoot
	s.Spotlight = &fakeSpotlight{ok: true, paths: []string{
		filepath.Join(root, filepath.FromSlash(fixtureDocs[4])),
	}}
	got, err := s.Find(context.Background(), Query{Text: "INV-2026-0417"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(relPaths(got), []string{fixtureDocs[0]}) {
		t.Errorf("got %q, want the Acme invoice", relPaths(got))
	}
}

// TestUnfiledDocumentsAreVisible is F4: a file outside every category folder is
// still found, still counted and still lintable. Making it invisible is the
// worst outcome for a tool whose job is keeping the vault honest.
func TestUnfiledDocumentsAreVisible(t *testing.T) {
	cfg := tempVault(t)
	mk := func(rel string) {
		p := filepath.Join(cfg.VaultRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("dropped-at-the-root.pdf")
	mk("Unsorted/holiday-scan.pdf")
	mk("Financial/Alex-Rao/Invoice_Alex-Rao_Acme_2026.txt")
	// Vault plumbing at the root stays invisible.
	mk("INDEX.md")
	mk("AGENTS.md")
	mk("README.md")
	mk("vault.log")

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Financial/Alex-Rao/Invoice_Alex-Rao_Acme_2026.txt",
		"Unsorted/holiday-scan.pdf",
		"dropped-at-the-root.pdf",
	}
	if !reflect.DeepEqual(relPaths(tree.Documents), want) {
		t.Fatalf("got %q want %q", relPaths(tree.Documents), want)
	}
	for _, d := range tree.Documents {
		wantCategory := ""
		if d.RelPath == want[0] {
			wantCategory = "financial"
		}
		if d.Category != wantCategory {
			t.Errorf("%s: category = %q, want %q", d.RelPath, d.Category, wantCategory)
		}
	}
	// A README inside a category folder is a document like any other; only the
	// vault root's own meta-documents are plumbing.
	mk("Financial/README.md")
	tree, err = s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Documents) != 4 {
		t.Errorf("a README inside a category should be a document: %q", relPaths(tree.Documents))
	}
}

// TestNestedCategoriesAreAttributedToTheMostSpecific is F8: with one category
// folder inside another, files under the inner one belong to the inner
// category, and neither category's folder is walked twice.
func TestNestedCategoriesAreAttributedToTheMostSpecific(t *testing.T) {
	dir := t.TempDir()
	yaml := "version: 1\nvault_root: .\npeople:\n  - name: Alex Rao\n" +
		"structure:\n  financial:\n    path: Financial\n    layout: \"{Owner}\"\n" +
		"  tax:\n    path: Financial/Tax\n    layout: \"{Owner}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "vault.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(filepath.Join(dir, "vault.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"Financial/Alex-Rao/Invoice_Alex-Rao_Acme_2026.txt",
		"Financial/Tax/Alex-Rao/Tax-Return_Alex-Rao_HMRC_2026.txt",
	} {
		p := filepath.Join(cfg.VaultRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Documents) != 2 {
		t.Fatalf("each file must be visited exactly once, got %q", relPaths(tree.Documents))
	}
	got := map[string]string{}
	for _, d := range tree.Documents {
		got[d.RelPath] = d.Category
	}
	want := map[string]string{
		"Financial/Alex-Rao/Invoice_Alex-Rao_Acme_2026.txt":        "financial",
		"Financial/Tax/Alex-Rao/Tax-Return_Alex-Rao_HMRC_2026.txt": "tax",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("categories = %v, want %v", got, want)
	}
}

// TestStagingFolderIsSkippedByItsConfiguredPath is F7: the staging and manifest
// folders are recognised through cfg.StagingDir()/cfg.ManifestDir() rather than
// through a string literal, so a future change to either path keeps working.
// (Neither is configurable in vault.yaml today, which is why this test can only
// assert the behaviour, not vary the name.)
func TestStagingFolderIsSkippedByItsConfiguredPath(t *testing.T) {
	cfg := tempVault(t)
	staged := filepath.Join(cfg.StagingDir(), "20260101-000000", "superseded.txt")
	if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Documents) != 0 {
		t.Errorf("staged files are not documents: %q", relPaths(tree.Documents))
	}
}
