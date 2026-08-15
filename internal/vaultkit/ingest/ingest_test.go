package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/audit"
	"github.com/getkagaz/kagaz/internal/vaultkit/classify"
	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/conventions"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/ocr"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// fixtureExtractor replays recorded OCR output instead of running pdftotext,
// Vision or Ollama. Every test in this package uses it, which is what lets the
// pipeline be tested end to end on Linux CI where none of those exist.
type fixtureExtractor struct {
	text   map[string]string
	engine string
}

func (f fixtureExtractor) Extract(_ context.Context, path string) (ocr.Result, error) {
	body, ok := f.text[filepath.Base(path)]
	if !ok {
		return ocr.Result{Engine: "none"}, ocr.ErrNoText
	}
	engine := f.engine
	if engine == "" {
		engine = "pdftotext"
	}
	return ocr.Result{Text: body, Engine: engine, Pages: 1}, nil
}

const testVaultYAML = `
version: 1
vault_root: %ROOT%
fiscal_year:
  start_month: 1
people:
  - name: Alex Rao
    tag: alex-rao
  - name: Sam Rao
    tag: sam-rao
tags:
  companies:
    - acme-corp
  fiscal_years:
    - fy2024
    - fy2025
    - fy2026
structure:
  financial:
    path: Financial
    layout: "{Owner}/{FY}"
  identity:
    path: Identity
    shared: _Shared
    layout: "{Owner}"
  insurance:
    path: Insurance
    layout: "{Owner}"
classify:
  engine: rules
`

// fixedNow is the clock every test runs on, so a year inferred from an mtime
// and a manifest filename are both deterministic.
var fixedNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// testPipeline builds a real pipeline -- real catalog, real conventions, real
// rules classifier, real move engine -- over a temporary vault, with only the
// OCR tier faked.
func testPipeline(t *testing.T, sources map[string]string) (*Pipeline, string, string) {
	t.Helper()

	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	inbox := filepath.Join(root, "inbox")
	for _, d := range []string{vault, inbox} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Parse([]byte(strings.ReplaceAll(testVaultYAML, "%ROOT%", vault)))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}

	text := map[string]string{}
	for name, fixture := range sources {
		body := readFixture(t, fixture)
		if err := os.WriteFile(filepath.Join(inbox, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		// A fixed mtime makes the "year came from the file's mtime" path
		// assertable rather than dependent on when the test ran.
		if err := os.Chtimes(filepath.Join(inbox, name), fixedNow, fixedNow); err != nil {
			t.Fatal(err)
		}
		text[name] = body
	}

	cat, err := doctypes.Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	names, err := conventions.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine := move.New(cfg.ManifestDir(), cfg.StagingDir())
	engine.Now = func() time.Time { return fixedNow }

	p := &Pipeline{
		Cfg:        cfg,
		Catalog:    cat,
		Names:      names,
		Vocab:      tags.NewVocabulary(cfg),
		Extractor:  fixtureExtractor{text: text},
		Classifier: classify.New(cfg, cat),
		Engine:     engine,
		Audit:      audit.Open(cfg.AuditLogPath()),
		Now:        func() time.Time { return fixedNow },
	}
	return p, inbox, vault
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "ocr", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(data)
}

// snapshot records every file under dir with its size and content hash.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		sum, err := move.SHA256(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[rel] = sum
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func standardSources() map[string]string {
	return map[string]string{
		"scan 2024-03-02 acme corp invoice.pdf": "invoice-acme.txt",
		"Sam Rao passport scan.pdf":             "passport-sam.txt",
		"globex policy.pdf":                     "policy-shared.txt",
		"IMG_0042.pdf":                          "gibberish.txt",
	}
}

func analyze(t *testing.T, p *Pipeline, inbox string) []Proposal {
	t.Helper()
	props, err := p.Analyze(context.Background(), []string{inbox})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return props
}

func find(t *testing.T, props []Proposal, base string) Proposal {
	t.Helper()
	for _, p := range props {
		if filepath.Base(p.Source) == base {
			return p
		}
	}
	t.Fatalf("no proposal for %s", base)
	return Proposal{}
}

// TestAnalyzeMutatesNothing is the load-bearing test of this package. Analyze
// must be readable end to end without a single write: no move, no sidecar, no
// tag, no folder, not even an empty vault subdirectory.
func TestAnalyzeMutatesNothing(t *testing.T) {
	p, inbox, vault := testPipeline(t, standardSources())

	beforeInbox := snapshot(t, inbox)
	beforeVault := snapshot(t, vault)

	props := analyze(t, p, inbox)
	if len(props) != 4 {
		t.Fatalf("got %d proposals, want 4", len(props))
	}

	if got := snapshot(t, inbox); len(got) != len(beforeInbox) {
		t.Errorf("Analyze changed the inbox: %v -> %v", beforeInbox, got)
	} else {
		for k, v := range beforeInbox {
			if got[k] != v {
				t.Errorf("Analyze modified %s", k)
			}
		}
	}
	if got := snapshot(t, vault); len(got) != len(beforeVault) {
		t.Errorf("Analyze wrote into the vault: %v", got)
	}

	// Not even the manifest, staging or audit paths may appear.
	for _, path := range []string{p.Cfg.ManifestDir(), p.Cfg.StagingDir(), p.Cfg.AuditLogPath()} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("Analyze created %s", path)
		}
	}
	// And no sidecar next to any source.
	entries, _ := os.ReadDir(inbox)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("Analyze wrote %s next to the source", e.Name())
		}
	}
}

func TestAnalyzeProposalContents(t *testing.T) {
	p, inbox, vault := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)

	t.Run("invoice: owner from text, year and identifier from extracted fields", func(t *testing.T) {
		got := find(t, props, "scan 2024-03-02 acme corp invoice.pdf")
		if got.Skip {
			t.Fatalf("skipped: %s", got.SkipReason)
		}
		if got.DocType != "invoice" || got.Category != "financial" {
			t.Fatalf("doctype/category = %s/%s, want invoice/financial", got.DocType, got.Category)
		}
		if len(got.Owners) != 1 || got.Owners[0] != "Alex Rao" {
			t.Fatalf("owners = %v, want [Alex Rao]", got.Owners)
		}
		if got.Why.Owners[0].Source != SourceText {
			t.Errorf("owner source = %s, want %s (the name is in the text, not the file name)", got.Why.Owners[0].Source, SourceText)
		}
		if got.Year != 2024 {
			t.Errorf("year = %d, want 2024 from the extracted date", got.Year)
		}
		if got.Why.Year.Source != SourceField {
			t.Errorf("year source = %s, want %s", got.Why.Year.Source, SourceField)
		}
		if got.Identifier != "INV-2024-0912" {
			t.Errorf("identifier = %q, want the extracted invoice_number", got.Identifier)
		}
		if got.Why.Identifier.Source != SourceField {
			t.Errorf("identifier source = %s, want %s", got.Why.Identifier.Source, SourceField)
		}
		want := filepath.Join(vault, "Financial", "Alex-Rao", "FY 2024", "Invoice_Alex-Rao_INV-2024-0912_2024.pdf")
		if got.Dest != want {
			t.Errorf("dest = %q, want %q", got.Dest, want)
		}
		if !contains(got.Tags, "alex-rao") || !contains(got.Tags, "fy2024") || !contains(got.Tags, "active") {
			t.Errorf("tags = %v, want alex-rao, fy2024 and active", got.Tags)
		}
		if got.Guessed() {
			t.Errorf("proposal flagged as guessed although every value came from an extracted field")
		}
	})

	t.Run("passport: owner from the file name, year guessed from mtime", func(t *testing.T) {
		got := find(t, props, "Sam Rao passport scan.pdf")
		if got.Skip {
			t.Fatalf("skipped: %s", got.SkipReason)
		}
		if got.DocType != "passport" {
			t.Fatalf("doctype = %s, want passport", got.DocType)
		}
		if len(got.Owners) != 1 || got.Owners[0] != "Sam Rao" {
			t.Fatalf("owners = %v, want [Sam Rao]", got.Owners)
		}
		if got.Why.Owners[0].Source != SourceFilename {
			t.Errorf("owner source = %s, want %s", got.Why.Owners[0].Source, SourceFilename)
		}
		if got.Year != fixedNow.Year() {
			t.Errorf("year = %d, want the file's mtime year %d", got.Year, fixedNow.Year())
		}
		if got.Why.Year.Source != SourceModTime {
			t.Fatalf("year source = %s, want %s", got.Why.Year.Source, SourceModTime)
		}
		// The mtime year is a guess and the explanation has to say so, in the
		// words a person reads before approving.
		if !strings.Contains(got.Why.Year.Detail, "guess") {
			t.Errorf("mtime year is not described as a guess: %q", got.Why.Year.Detail)
		}
		if !got.Guessed() {
			t.Error("a proposal whose year came from the mtime should be flagged as guessed")
		}
		if got.Identifier != "X1234567" {
			t.Errorf("identifier = %q, want the extracted passport_number", got.Identifier)
		}
	})

	t.Run("policy: no owner matched, filed unowned", func(t *testing.T) {
		got := find(t, props, "globex policy.pdf")
		if got.Skip {
			t.Fatalf("skipped: %s", got.SkipReason)
		}
		if got.DocType != "insurance-policy" {
			t.Fatalf("doctype = %s, want insurance-policy", got.DocType)
		}
		if len(got.Owners) != 0 {
			t.Fatalf("owners = %v, want none", got.Owners)
		}
		if len(got.Why.Owners) == 0 || got.Why.Owners[0].Source != SourceNone {
			t.Fatalf("owner rationale = %+v, want a 'none' reason", got.Why.Owners)
		}
		if !strings.Contains(got.Why.Owners[0].Detail, "Alex Rao") {
			t.Errorf("the no-owner explanation should name who was looked for: %q", got.Why.Owners[0].Detail)
		}
		// The filename pattern requires {Names}, so the name borrows the
		// shared marker -- and the preview has to say so.
		if !strings.Contains(filepath.Base(got.Dest), SharedMarker) {
			t.Errorf("file name = %q, want the shared marker", filepath.Base(got.Dest))
		}
		explained := false
		for _, r := range got.Why.Owners {
			if strings.Contains(r.Detail, SharedMarker) {
				explained = true
			}
		}
		if !explained {
			t.Errorf("the shared-marker substitution is not explained: %+v", got.Why.Owners)
		}
		// insurance has no shared folder, so an unowned document lands
		// directly in the category.
		want := filepath.Join(vault, "Insurance")
		if filepath.Dir(got.Dest) != want {
			t.Errorf("dest dir = %q, want %q", filepath.Dir(got.Dest), want)
		}
	})

	t.Run("gibberish: unclassified is skipped, never filed under a guess", func(t *testing.T) {
		got := find(t, props, "IMG_0042.pdf")
		if !got.Skip {
			t.Fatalf("an unclassified document produced a destination: %s", got.Dest)
		}
		if got.Dest != "" {
			t.Errorf("dest = %q, want empty for an unclassified document", got.Dest)
		}
		if got.DocType != doctypes.Unclassified {
			t.Errorf("doctype = %s, want %s", got.DocType, doctypes.Unclassified)
		}
		if got.SkipReason == "" {
			t.Error("no skip reason given")
		}
	})
}

// TestAnalyzeIsStableAndNumbered checks the property batch approval depends on:
// the same input yields the same proposals in the same order, numbered from 1,
// so the number a user types means the same document it did on screen.
func TestAnalyzeIsStableAndNumbered(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())

	first := analyze(t, p, inbox)
	second := analyze(t, p, inbox)
	if len(first) != len(second) {
		t.Fatalf("Analyze is not stable: %d then %d proposals", len(first), len(second))
	}
	for i := range first {
		if first[i].Index != i+1 {
			t.Errorf("proposal %d has Index %d", i, first[i].Index)
		}
		if first[i].Source != second[i].Source || first[i].Dest != second[i].Dest {
			t.Errorf("proposal %d differs between runs: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestAnalyzeSkipsSidecarsAndDotfiles(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	if err := os.WriteFile(filepath.Join(inbox, ".hidden.pdf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, ".globex policy.pdf.meta.yaml"), []byte("doctype: invoice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	props := analyze(t, p, inbox)
	for _, prop := range props {
		base := filepath.Base(prop.Source)
		if strings.HasPrefix(base, ".") {
			t.Errorf("analysed dotfile %s", base)
		}
	}
	if len(props) != 4 {
		t.Fatalf("got %d proposals, want 4", len(props))
	}
}

func TestAnalyzeRejectsAMissingPath(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	if _, err := p.Analyze(context.Background(), []string{filepath.Join(inbox, "nope.pdf")}); err == nil {
		t.Fatal("Analyze accepted a path that does not exist")
	}
}

// TestAnalyzeSurvivesAnUnreadableFile checks the degradation rule: one file
// with no extractable text costs that file a proposal, not the batch.
func TestAnalyzeSurvivesAnUnreadableFile(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	// A file the fixture extractor has no recording for stands in for a scan
	// nothing on this machine can read.
	if err := os.WriteFile(filepath.Join(inbox, "unreadable.pdf"), []byte("%PDF-1.4 broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	props := analyze(t, p, inbox)
	if len(props) != 5 {
		t.Fatalf("got %d proposals, want 5", len(props))
	}
	got := find(t, props, "unreadable.pdf")
	if !got.Skip {
		t.Fatal("a file with no extractable text produced a destination")
	}
	if len(got.Warnings) == 0 {
		t.Error("no warning explaining the extraction failure")
	}
	// The rest of the batch is unaffected.
	if find(t, props, "scan 2024-03-02 acme corp invoice.pdf").Skip {
		t.Error("one unreadable file skipped a readable one")
	}
}

func TestProposeTagsDropsTagsOutsideTheVocabulary(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)
	got := find(t, props, "globex policy.pdf")

	// "pol-88213" is derived from the identifier and is not vocabulary, so it
	// must be withheld and reported rather than applied.
	for _, tag := range got.Tags {
		if tag == "pol-88213" {
			t.Fatal("a tag outside the vocabulary was applied")
		}
	}
	found := false
	for _, d := range got.DroppedTags {
		if d.Tag == "pol-88213" {
			found = true
			if !strings.Contains(d.Reason, "vault.yaml") {
				t.Errorf("dropped-tag reason does not say how to fix it: %q", d.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("dropped tags = %+v, want the out-of-vocabulary tag reported", got.DroppedTags)
	}
}

func TestPreviewShowsTheReasoning(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)
	out := PreviewBatch(props)

	for _, want := range []string{
		"1. ",
		"->",
		"invoice",
		"owner Alex Rao",
		"why",
		"SKIP",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview is missing %q:\n%s", want, out)
		}
	}
	// Every non-skipped proposal explains its doctype, year and identifier.
	for _, prop := range props {
		if prop.Skip {
			continue
		}
		if len(prop.Explain()) < 3 {
			t.Errorf("proposal %d explains only %d values: %v", prop.Index, len(prop.Explain()), prop.Explain())
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
