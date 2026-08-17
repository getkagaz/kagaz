package ocr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTextUtilDegradesWithoutTheTool: on a machine with no textutil -- Linux
// CI, every time -- a `.doc` must produce an error that names the missing tool
// and not one that blames the document. Constraint 9 is the whole point of this
// test.
func TestTextUtilDegradesWithoutTheTool(t *testing.T) {
	stubLookPath(t, nil)

	path := writeTemp(t, "letter.doc", []byte("not really a doc"))
	_, err := (&TextUtil{}).Extract(context.Background(), path)
	if !errors.Is(err, ErrNoTextUtil) {
		t.Fatalf("error = %v, want it to wrap ErrNoTextUtil", err)
	}
	for _, want := range []string{"textutil", "Word 97-2003", "not on this machine"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	if (&TextUtil{}).Available() {
		t.Error("Available is true with no textutil on PATH")
	}
	if !strings.Contains((&TextUtil{}).detail(), "not found") {
		t.Errorf("the doctor line does not say the tool is missing: %q", (&TextUtil{}).detail())
	}
}

// TestTextUtilClaimsItsExtensionsRegardless: Handles must not depend on
// Available, or a `.doc` on Linux would fall through to the OCR tiers and die a
// second per page later as a generic "no text".
func TestTextUtilClaimsItsExtensionsRegardless(t *testing.T) {
	stubLookPath(t, nil)
	tu := &TextUtil{}
	for _, ext := range TextUtilExtensions {
		if !tu.Handles("/tmp/doc" + ext) {
			t.Errorf("Handles(%s) = false", ext)
		}
	}
	if tu.Handles("/tmp/doc.docx") {
		t.Error("Handles claimed .docx, which belongs to the Office runner")
	}
}

// TestTextUtilRoundTrip converts text to a real legacy `.doc` with textutil and
// reads it back through the runner. It is a genuine round trip -- the fixture is
// produced by the same tool a user's Word would have produced one with -- and it
// is skipped wherever textutil is absent.
func TestTextUtilRoundTrip(t *testing.T) {
	bin, err := exec.LookPath("textutil")
	if err != nil {
		t.Skip("textutil is not installed on this machine")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "invoice.txt")
	const body = "INVOICE\nInvoice Number: INV-2019-0042\nTotal Amount Due: 45,300.00 INR\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(dir, "invoice.doc")
	if out, err := exec.Command(bin, "-convert", "doc", "-output", doc, src).CombinedOutput(); err != nil {
		t.Fatalf("building the fixture: %v: %s", err, out)
	}

	res, err := (&TextUtil{}).Extract(context.Background(), doc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Engine != "textutil" {
		t.Errorf("Engine = %q, want textutil", res.Engine)
	}
	for _, want := range []string{"INVOICE", "INV-2019-0042", "45,300.00 INR"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text is missing %q:\n%s", want, res.Text)
		}
	}

	// The same file is a compound file, so prove the two tiers do not fight
	// over it: LegacyOffice must not claim `.doc`.
	if (&LegacyOffice{}).Handles(doc) {
		t.Error("LegacyOffice claimed a .doc; textutil owns that format")
	}
}

// TestExtractorRoutesLegacyFormats: dispatch must reach the new tiers before
// the PDF and Vision ones, and must reach the right one for each extension.
func TestExtractorRoutesLegacyFormats(t *testing.T) {
	stubLookPath(t, nil) // no pdftotext, no textutil, no helper

	e := &Extractor{
		Text:     &PlainText{},
		Office:   &Office{},
		TextUtil: &TextUtil{},
		Legacy:   &LegacyOffice{},
		PDF:      &PDFToText{},
		Vision:   &Vision{},
		Ollama:   &Ollama{Enabled: "false"},
	}

	// A `.doc` reaches textutil, which names the missing tool.
	_, err := e.Extract(context.Background(), writeTemp(t, "a.doc", []byte("x")))
	if !errors.Is(err, ErrNoTextUtil) {
		t.Errorf(".doc did not reach the textutil tier: %v", err)
	}

	// An `.xls` reaches the in-process parser, which always works.
	xls := writeTemp(t, "a.xls", buildCFB("Workbook", buildWorkbook(), cfbOptions{}))
	res, err := e.Extract(context.Background(), xls)
	if err != nil {
		t.Fatalf(".xls: %v", err)
	}
	if res.Engine != "legacyoffice" {
		t.Errorf("Engine = %q, want legacyoffice", res.Engine)
	}
}

// TestDescribeReportsTheNewTiers: `kagaz doctor` must list both, with the
// pure-Go one always available and the shelled-out one honest about the tool.
func TestDescribeReportsTheNewTiers(t *testing.T) {
	stubLookPath(t, nil)

	e := &Extractor{
		Text:     &PlainText{},
		Office:   &Office{},
		TextUtil: &TextUtil{},
		Legacy:   &LegacyOffice{},
		PDF:      &PDFToText{},
		Vision:   &Vision{},
		Ollama:   &Ollama{},
	}
	got := map[string]bool{}
	for _, s := range e.Describe() {
		got[s.Name] = s.Available
	}
	if avail, ok := got["legacyoffice"]; !ok || !avail {
		t.Errorf("legacyoffice is missing or reported unavailable: %v", got)
	}
	if avail, ok := got["textutil"]; !ok {
		t.Errorf("textutil is missing from doctor output: %v", got)
	} else if avail {
		t.Error("textutil reported available with nothing on PATH")
	}
}

// TestTextUtilRefusesALeadingDashFilename: a filename beginning with a dash
// would be read by textutil as an option rather than as a file.
//
// Nothing reaches Extract with such a path today -- ingest.collect runs every
// path through filepath.Abs -- but that is a property of code two packages
// away, and the argv of a subprocess is not a thing to secure at a distance.
func TestTextUtilRefusesALeadingDashFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "-convert.rtf")
	if err := os.WriteFile(path, []byte("{\\rtf1 hello}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The guard must fire before the tool is even looked up, so the test is the
	// same on Linux CI and on a Mac.
	_, err := (&TextUtil{}).Extract(context.Background(), path)
	if err == nil {
		t.Fatal("a filename that would be read as an option was accepted")
	}
	if !strings.Contains(err.Error(), "option") {
		t.Errorf("the refusal does not explain the dash: %v", err)
	}
}

// TestTextUtilAcceptsOnlyTheSystemPath: textutil ships inside macOS and is not
// a tool a user installs, so a `textutil` earlier on $PATH is somebody else's
// program with the same name -- and the error message this tier prints promises
// /usr/bin/textutil by name.
func TestTextUtilAcceptsOnlyTheSystemPath(t *testing.T) {
	stubLookPath(t, map[string]string{"textutil": "/opt/homebrew/bin/textutil"})
	if (&TextUtil{}).Available() {
		t.Error("a textutil somewhere other than /usr/bin was accepted")
	}
	stubLookPath(t, map[string]string{"textutil": textUtilPath})
	if !(&TextUtil{}).Available() {
		t.Errorf("%s was not accepted", textUtilPath)
	}
}
