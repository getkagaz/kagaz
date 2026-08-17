package ingest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
)

// analyzeWith is analyze with overrides, failing the test on a rejection.
func analyzeWith(t *testing.T, p *Pipeline, inbox string, ov Overrides) []Proposal {
	t.Helper()
	props, err := p.AnalyzeWith(context.Background(), []string{inbox}, ov)
	if err != nil {
		t.Fatalf("AnalyzeWith(%+v): %v", ov, err)
	}
	return props
}

// TestSetDocTypeFilesADocumentNothingCouldClassify is the whole point of the
// feature: gibberish.txt is what the corpus's 136 skipped documents look like,
// and without a way to say what one is, a skipped document is a dead end.
func TestSetDocTypeFilesADocumentNothingCouldClassify(t *testing.T) {
	p, inbox, vault := testPipeline(t, map[string]string{"IMG_0042.pdf": "gibberish.txt"})

	before := find(t, analyze(t, p, inbox), "IMG_0042.pdf")
	if !before.Skip {
		t.Fatalf("IMG_0042.pdf was classified as %q; this test needs a document nothing can classify", before.DocType)
	}

	got := find(t, analyzeWith(t, p, inbox, Overrides{
		DocType: "insurance-policy", Owners: []string{"sam-rao"}, Identifier: "Globex", Year: 2024,
	}), "IMG_0042.pdf")

	if got.Skip {
		t.Fatalf("still skipped after --set-doctype: %s", got.SkipReason)
	}
	if got.DocType != "insurance-policy" {
		t.Errorf("doctype = %q, want insurance-policy", got.DocType)
	}
	// The category is the catalog's, never a flag's.
	if got.Category != "insurance" {
		t.Errorf("category = %q, want insurance (from the catalog)", got.Category)
	}
	if len(got.Owners) != 1 || got.Owners[0] != "Sam Rao" {
		t.Errorf("owners = %v, want [Sam Rao] resolved from the tag", got.Owners)
	}
	if got.Year != 2024 || got.Identifier != "Globex" {
		t.Errorf("year/identifier = %d/%q, want 2024/Globex", got.Year, got.Identifier)
	}
	if !strings.HasPrefix(got.Dest, vault) {
		t.Errorf("destination %q is not in the vault", got.Dest)
	}
	// Extraction still ran: only the inference was overridden.
	if got.OCREngine == "" || got.OCREngine == "none" || got.Text == "" {
		t.Errorf("ocr did not run: engine %q, %d bytes of text", got.OCREngine, len(got.Text))
	}
}

// TestHumanProvenanceIsRecordedEverywhereItIsRead covers the rule that matters
// most: a doctype a person assigned must never be readable as one a model
// produced -- not in the proposal, not in the preview, not in the sidecar the
// user reads back months later.
func TestHumanProvenanceIsRecordedEverywhereItIsRead(t *testing.T) {
	p, inbox, _ := testPipeline(t, map[string]string{"IMG_0042.pdf": "gibberish.txt"})
	prop := find(t, analyzeWith(t, p, inbox, Overrides{
		DocType: "insurance-policy", Owners: []string{"Sam Rao"},
	}), "IMG_0042.pdf")

	if prop.Classifier != ClassifierHuman {
		t.Errorf("classifier = %q, want %q", prop.Classifier, ClassifierHuman)
	}
	if prop.Confidence != 0 {
		t.Errorf("confidence = %v, want 0: a person's decision is not a probability", prop.Confidence)
	}
	if prop.Why.DocType.Source != SourceHuman {
		t.Errorf("why.doctype.source = %q, want %q", prop.Why.DocType.Source, SourceHuman)
	}
	for _, want := range []string{"--set-doctype", "not inferred", "unclassified", "catalog"} {
		if !strings.Contains(prop.Why.DocType.Detail, want) {
			t.Errorf("why line %q does not say %q", prop.Why.DocType.Detail, want)
		}
	}
	if len(prop.Why.Owners) != 1 || !strings.Contains(prop.Why.Owners[0].Detail, "--set-owner") {
		t.Errorf("owner rationale does not say a person supplied it: %+v", prop.Why.Owners)
	}
	// The preview must not print a confidence a person never gave.
	preview := prop.Preview()
	if !strings.Contains(preview, "assigned by you") || strings.Contains(preview, "confidence") {
		t.Errorf("preview misreports a human assignment:\n%s", preview)
	}

	res, err := p.Execute([]Proposal{prop})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Filed) != 1 {
		t.Fatalf("filed %d documents, want 1", len(res.Filed))
	}
	meta, err := sidecar.Read(res.Filed[0].Dest)
	if err != nil {
		t.Fatalf("sidecar.Read: %v", err)
	}
	if meta.Classifier != ClassifierHuman {
		t.Errorf("sidecar classifier = %q, want %q", meta.Classifier, ClassifierHuman)
	}
	if meta.Confidence != 0 {
		t.Errorf("sidecar confidence = %v, want none", meta.Confidence)
	}
	raw, err := os.ReadFile(sidecar.Path(res.Filed[0].Dest))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "confidence:") {
		t.Errorf("the sidecar records a confidence for a human assignment:\n%s", raw)
	}
}

// TestOverridesLeaveEveryOtherInferenceAlone pins the rule that only what was
// stated is replaced. A stated doctype must not cost the user the year, owner
// and fields the pipeline could still work out for itself.
func TestOverridesLeaveEveryOtherInferenceAlone(t *testing.T) {
	p, inbox, _ := testPipeline(t, map[string]string{
		"scan 2024-03-02 acme corp invoice.pdf": "invoice-acme.txt",
	})
	got := find(t, analyzeWith(t, p, inbox, Overrides{DocType: "receipt"}), "scan 2024-03-02 acme corp invoice.pdf")

	if got.DocType != "receipt" {
		t.Fatalf("doctype = %q, want receipt", got.DocType)
	}
	if len(got.Owners) == 0 {
		t.Errorf("owner inference did not run: %+v", got.Why.Owners)
	}
	if got.Year != 2024 {
		t.Errorf("year = %d, want 2024 inferred from the document", got.Year)
	}
	if len(got.Fields) == 0 {
		t.Errorf("field extraction did not run")
	}
	if got.Why.Year.Source == SourceHuman || got.Why.Identifier.Source == SourceHuman {
		t.Errorf("an inference was labelled as human-stated: %+v", got.Why)
	}
}

func TestOverridesAreValidatedBeforeAnythingIsRead(t *testing.T) {
	p, inbox, _ := testPipeline(t, map[string]string{"IMG_0042.pdf": "gibberish.txt"})

	tests := []struct {
		name string
		ov   Overrides
		want []string
	}{
		{
			name: "unknown doctype names real alternatives",
			ov:   Overrides{DocType: "invioce"},
			want: []string{"invioce", "not a doctype in this vault's catalog", "invoice", "vault.yaml"},
		},
		{
			name: "unclassified is not a filing decision",
			ov:   Overrides{DocType: "unclassified"},
			want: []string{"unclassified", "Name a real doctype"},
		},
		{
			name: "unknown owner names the vault's people",
			ov:   Overrides{DocType: "receipt", Owners: []string{"Robin Fox"}},
			want: []string{"Robin Fox", "Alex Rao", "Sam Rao"},
		},
		{
			name: "a year must be four digits",
			ov:   Overrides{DocType: "receipt", Year: 24},
			want: []string{"four-digit year"},
		},
		{
			name: "a blank identifier is a typo, not an instruction",
			ov:   Overrides{DocType: "receipt", Identifier: "   "},
			want: []string{"--set-identifier is blank"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.AnalyzeWith(context.Background(), []string{inbox}, tc.ov)
			if err == nil {
				t.Fatalf("AnalyzeWith(%+v) was accepted", tc.ov)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestOwnersAreCanonicalisedAndDeduplicated: a tag and a display name for the
// same person are the same owner, and the filename must use the configured
// display name whichever the user typed.
func TestOwnersAreCanonicalisedAndDeduplicated(t *testing.T) {
	p, inbox, _ := testPipeline(t, map[string]string{"IMG_0042.pdf": "gibberish.txt"})
	got := find(t, analyzeWith(t, p, inbox, Overrides{
		DocType: "insurance-policy", Owners: []string{"sam-rao", "Sam Rao", "alex-rao"},
	}), "IMG_0042.pdf")

	want := []string{"Sam Rao", "Alex Rao"}
	if len(got.Owners) != len(want) {
		t.Fatalf("owners = %v, want %v", got.Owners, want)
	}
	for i, w := range want {
		if got.Owners[i] != w {
			t.Errorf("owner %d = %q, want %q", i, got.Owners[i], w)
		}
	}
}
