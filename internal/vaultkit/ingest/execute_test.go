package ingest

import (
	"context"
	"encoding/json"
	"errors"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/move"
	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
)

func TestExecuteFilesTheBatchUnderOneManifest(t *testing.T) {
	p, inbox, vault := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)

	res, err := p.Execute(props)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Three classified documents move; the unclassified one does not.
	if len(res.Filed) != 3 {
		t.Fatalf("filed %d documents, want 3: %+v", len(res.Filed), res.Filed)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped %d, want 1", len(res.Skipped))
	}

	// One manifest for the whole batch, with a row per moved document.
	manPath := res.ManifestPath()
	if manPath == "" {
		t.Fatal("no manifest was written")
	}
	entries, err := os.ReadDir(p.Cfg.ManifestDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d manifests written, want exactly 1: %v", len(entries), entries)
	}
	man, err := move.ReadManifest(manPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Rows) != 3 {
		t.Fatalf("manifest has %d rows, want 3", len(man.Rows))
	}
	if man.Op != OpName {
		t.Errorf("manifest op = %q, want %q", man.Op, OpName)
	}

	// Each document is where it was proposed, and the source is staged, never
	// deleted.
	for _, prop := range res.Filed {
		if _, err := os.Stat(prop.Dest); err != nil {
			t.Errorf("%s was not filed: %v", prop.Dest, err)
		}
		if _, err := os.Stat(prop.Source); err == nil {
			t.Errorf("%s is still at its source path", prop.Source)
		}
	}
	staged := 0
	_ = filepath.WalkDir(p.Cfg.StagingDir(), func(path string, d os.DirEntry, err error) error {
		if err == nil && d != nil && !d.IsDir() {
			staged++
		}
		return nil
	})
	if staged != 3 {
		t.Errorf("%d files in staging, want 3 (sources are staged, never deleted)", staged)
	}

	// The unclassified document is untouched in the inbox.
	if _, err := os.Stat(filepath.Join(inbox, "IMG_0042.pdf")); err != nil {
		t.Errorf("the skipped document was moved anyway: %v", err)
	}
	_ = vault
}

func TestExecuteWritesTheSidecar(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)
	res, err := p.Execute(props)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var invoice Proposal
	for _, prop := range res.Filed {
		if prop.DocType == "invoice" {
			invoice = prop
		}
	}
	if invoice.Dest == "" {
		t.Fatal("the invoice was not filed")
	}

	meta, err := sidecar.Read(invoice.Dest)
	if err != nil {
		t.Fatalf("sidecar.Read: %v", err)
	}
	if meta == nil {
		t.Fatal("no sidecar was written next to the filed document")
	}
	if meta.OCREngine != "pdftotext" {
		t.Errorf("ocr_engine = %q, want pdftotext", meta.OCREngine)
	}
	if meta.Classifier != "rules" {
		t.Errorf("classifier = %q, want rules", meta.Classifier)
	}
	if meta.DocType != "invoice" || meta.Category != "financial" {
		t.Errorf("doctype/category = %s/%s", meta.DocType, meta.Category)
	}
	if meta.SourceSHA != invoice.SourceSHA || len(meta.SourceSHA) != 64 {
		t.Errorf("source_sha256 = %q, want the analysed hash %q", meta.SourceSHA, invoice.SourceSHA)
	}
	if !strings.Contains(meta.Text, "INV-2024-0912") {
		t.Errorf("sidecar text does not carry the extracted OCR text")
	}
	if meta.Fields["invoice_number"] != "INV-2024-0912" {
		t.Errorf("fields = %v, want the extracted invoice_number", meta.Fields)
	}
	if len(meta.Owners) != 1 || meta.Owners[0] != "Alex Rao" {
		t.Errorf("owners = %v", meta.Owners)
	}
	if meta.Year != 2024 {
		t.Errorf("year = %d, want 2024", meta.Year)
	}

	// The sidecar is a hidden companion of the document, in the same folder.
	want := sidecar.Path(invoice.Dest)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("sidecar not at %s: %v", want, err)
	}
}

func TestExecuteWritesOneAuditLine(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)
	res, err := p.Execute(props)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	data, err := os.ReadFile(p.Cfg.AuditLogPath())
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d audit lines, want exactly 1 for the batch", len(lines))
	}
	var entry struct {
		Op       string            `json:"op"`
		Paths    []string          `json:"paths"`
		Manifest string            `json:"manifest"`
		Detail   map[string]string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("audit line is not JSON: %v", err)
	}
	if entry.Op != OpName {
		t.Errorf("op = %q, want %q", entry.Op, OpName)
	}
	if entry.Manifest != res.ManifestPath() {
		t.Errorf("manifest = %q, want %q", entry.Manifest, res.ManifestPath())
	}
	if len(entry.Paths) != 3 {
		t.Errorf("paths = %v, want the three filed documents", entry.Paths)
	}
	if entry.Detail["documents"] != "3" {
		t.Errorf("detail = %v", entry.Detail)
	}
	// Document text never reaches the log. (Paths do, and a conventional path
	// legitimately contains the identifier -- that is the naming grammar, not
	// a leak.)
	for _, secret := range []string{"Consulting services", "Payment is due", "Sum Insured", "Given Names"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("the audit line leaked document text: %q", secret)
		}
	}
}

// TestExecuteIsReversible checks the promise the manifest exists to keep.
func TestExecuteIsReversible(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	before := snapshot(t, inbox)

	props := analyze(t, p, inbox)
	res, err := p.Execute(props)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	man, err := move.ReadManifest(res.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Engine.Rollback(man); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	after := snapshot(t, inbox)
	for name, sum := range before {
		if after[name] != sum {
			t.Errorf("%s did not come back to the inbox intact (%q vs %q)", name, after[name], sum)
		}
	}
}

// TestExecuteSkipsProposalsMarkedSkip proves the CLI cannot accidentally file
// something Analyze refused to place, even by passing the whole batch through.
func TestExecuteSkipsProposalsMarkedSkip(t *testing.T) {
	p, inbox, _ := testPipeline(t, map[string]string{"IMG_0042.pdf": "gibberish.txt"})
	props := analyze(t, p, inbox)
	if len(props) != 1 || !props[0].Skip {
		t.Fatalf("expected one skipped proposal, got %+v", props)
	}

	res, err := p.Execute(props)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Filed) != 0 {
		t.Fatalf("filed %+v, want nothing", res.Filed)
	}
	if res.ManifestPath() != "" {
		t.Error("a manifest was written for a batch with nothing to do")
	}
	if _, err := os.Stat(p.Cfg.AuditLogPath()); err == nil {
		t.Error("an audit line was written for a batch with nothing to do")
	}
	if _, err := os.Stat(filepath.Join(inbox, "IMG_0042.pdf")); err != nil {
		t.Errorf("the skipped document was moved: %v", err)
	}
}

// TestExecutePartialBatchLeavesAReversibleManifest is the interrupted case.
// The second document's destination folder is blocked by a regular file, so
// the batch fails half way -- and what matters is that the manifest is on disk
// covering the whole planned batch, so `kagaz rollback` can put back what did
// move and report what did not.
func TestExecutePartialBatchLeavesAReversibleManifest(t *testing.T) {
	p, inbox, vault := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)

	// Order the batch so a good move happens first and the blocked one second.
	var ordered []Proposal
	for _, prop := range props {
		if prop.DocType == "invoice" {
			ordered = append(ordered, prop)
		}
	}
	var blocked Proposal
	for _, prop := range props {
		if prop.DocType == "passport" {
			blocked = prop
			ordered = append(ordered, prop)
		}
	}
	if len(ordered) != 2 {
		t.Fatalf("expected two proposals to order, got %d", len(ordered))
	}

	// A regular file where the passport's owner folder should be makes
	// MkdirAll fail for that document and only that document.
	blockedDir := filepath.Dir(blocked.Dest)
	if err := os.MkdirAll(filepath.Dir(blockedDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := p.Execute(ordered)
	if err == nil {
		t.Fatal("Execute returned no error for a batch it could not finish")
	}

	manPath := res.ManifestPath()
	if manPath == "" {
		t.Fatal("a partially executed batch left no manifest")
	}
	man, err := move.ReadManifest(manPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Rows) != 2 {
		t.Fatalf("manifest has %d rows, want 2 (the whole planned batch)", len(man.Rows))
	}

	// The first document really moved, and got its sidecar.
	if len(res.Filed) != 1 || res.Filed[0].DocType != "invoice" {
		t.Fatalf("filed = %+v, want just the invoice", res.Filed)
	}
	if meta, _ := sidecar.Read(res.Filed[0].Dest); meta == nil {
		t.Error("the document that did move has no sidecar")
	}

	// Rollback puts back what moved and reports what did not, without failing.
	rb, err := p.Engine.Rollback(man)
	if err != nil {
		t.Fatalf("Rollback of a partial manifest: %v", err)
	}
	if len(rb.Warnings) == 0 {
		t.Error("Rollback reported no warning for the row that never moved")
	}
	if _, err := os.Stat(filepath.Join(inbox, "scan 2024-03-02 acme corp invoice.pdf")); err != nil {
		t.Errorf("the moved document did not come back: %v", err)
	}
	_ = vault
}

// TestExecuteToleratesAFilesystemWithoutTags is Global Constraint 9 for this
// pipeline: on Linux CI (and many network mounts) extended attributes do not
// exist, and that must cost a warning, not the ingest.
func TestExecuteToleratesAFilesystemWithoutTags(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)

	res, err := p.Execute(props)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(res.Filed) != 3 {
		t.Fatalf("filed %d, want 3", len(res.Filed))
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "panic") {
			t.Errorf("unexpected warning: %s", w)
		}
	}
}

func TestExecuteWithoutAnEngineIsAnError(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)
	p.Engine = nil
	if _, err := p.Execute(props); err == nil || !strings.Contains(err.Error(), "move engine") {
		t.Fatalf("Execute without an engine = %v, want a clear error", err)
	}
}

// TestExecuteRefusesADestinationOutsideTheVault hardens the mutating half of
// the API against the caller it is about to get: a CLI that lets a user edit a
// proposal before approving it. An unchecked Dest is an arbitrary file write
// through a tool whose whole promise is that it only moves documents inside
// your vault.
func TestExecuteRefusesADestinationOutsideTheVault(t *testing.T) {
	p, inbox, vault := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)

	var good Proposal
	for _, prop := range props {
		if !prop.Skip {
			good = prop
			break
		}
	}
	if good.Dest == "" {
		t.Fatal("no usable proposal")
	}

	outside := filepath.Join(filepath.Dir(vault), "escaped.pdf")
	tests := []struct {
		name string
		dest string
		want string
	}{
		{"a sibling of the vault", outside, "outside the vault root"},
		{"a traversal out of the vault", filepath.Join(vault, "..", "escaped.pdf"), "outside the vault root"},
		{"an absolute path elsewhere", filepath.Join(t.TempDir(), "elsewhere.pdf"), "outside the vault root"},
		{"the vault root itself", vault, "outside the vault root"},
		{"a relative path", "Financial/x.pdf", "not an absolute path"},
		{"inside the staging area", filepath.Join(p.Cfg.StagingDir(), "x.pdf"), "reserves for its own bookkeeping"},
		{"inside the manifest directory", filepath.Join(p.Cfg.ManifestDir(), "x.pdf"), "reserves for its own bookkeeping"},
		{"a sidecar path", filepath.Join(vault, "Financial", ".x.pdf.meta.yaml"), "sidecar path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := good
			bad.Dest = tt.dest

			res, err := p.Execute([]Proposal{bad})
			if err == nil {
				t.Fatalf("Execute accepted destination %q", tt.dest)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
			// The refusal happens before the manifest is written, so nothing
			// at all was done.
			if res.ManifestPath() != "" {
				t.Error("a rejected batch still wrote a manifest")
			}
			if _, err := os.Stat(tt.dest); err == nil && tt.dest != vault {
				t.Errorf("something was written to %q", tt.dest)
			}
			if _, err := os.Stat(good.Source); err != nil {
				t.Errorf("the source was moved despite the refusal: %v", err)
			}
		})
	}
}

// TestExecuteRechecksTagsAgainstTheVocabulary: Analyze already filtered the
// tags, but a caller may have edited the proposal since, and an uncontrolled
// vocabulary is what makes saved searches unreliable.
func TestExecuteRechecksTagsAgainstTheVocabulary(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())
	props := analyze(t, p, inbox)

	edited := make([]Proposal, 0, len(props))
	for _, prop := range props {
		if !prop.Skip {
			prop.Tags = append(append([]string(nil), prop.Tags...), "not-in-the-vocabulary")
		}
		edited = append(edited, prop)
	}

	res, err := p.Execute(edited)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Filed) == 0 {
		t.Fatal("nothing was filed")
	}
	for _, prop := range res.Filed {
		for _, tag := range prop.Tags {
			if tag == "not-in-the-vocabulary" {
				t.Errorf("%s: an out-of-vocabulary tag survived to execution", filepath.Base(prop.Dest))
			}
		}
		found := false
		for _, d := range prop.DroppedTags {
			if d.Tag == "not-in-the-vocabulary" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the rejected tag was discarded silently instead of reported: %+v",
				filepath.Base(prop.Dest), prop.DroppedTags)
		}
	}
}

// TestAnalyzeReportsCancellation: a cancelled batch must say it was cancelled,
// not come back as N documents that look unreadable.
func TestAnalyzeReportsCancellation(t *testing.T) {
	p, inbox, _ := testPipeline(t, standardSources())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	props, err := p.Analyze(ctx, []string{inbox})
	if err == nil {
		t.Fatalf("a cancelled Analyze returned %d proposals and no error", len(props))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
	if props != nil {
		t.Errorf("a cancelled Analyze returned proposals: %+v", props)
	}
}
