package ocr

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zipEntry is one member of a fixture archive.
type zipEntry struct {
	name string
	body string
}

// writeZipFixture builds a real ZIP archive on disk from entries. Fixtures are
// built rather than committed so the tests carry no binary blobs and run
// identically on Linux CI, where no Office application exists.
func writeZipFixture(t *testing.T, name string, entries []zipEntry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return writeTemp(t, name, buf.Bytes())
}

const docxHeader = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`

const docxFooter = `</w:body></w:document>`

const sheetHeader = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`

const sheetFooter = `</sheetData></worksheet>`

const slideHeader = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"` +
	` xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree>`

const slideFooter = `</p:spTree></p:cSld></p:sld>`

// TestOfficeExtractsOOXML covers the three supported formats and the details
// that decide whether the extracted text is readable at all: paragraph
// boundaries, tabs and breaks, the shared string table, inline strings, and
// part order.
func TestOfficeExtractsOOXML(t *testing.T) {
	tests := []struct {
		name      string
		file      string
		entries   []zipEntry
		want      string
		wantPages int
	}{
		{
			name: "docx keeps paragraph boundaries between runs",
			file: "invoice.docx",
			entries: []zipEntry{{"word/document.xml", docxHeader +
				`<w:p><w:r><w:t>Invoice</w:t></w:r><w:r><w:t xml:space="preserve"> INV-2024-018</w:t></w:r></w:p>` +
				`<w:p><w:r><w:t>Total due</w:t><w:tab/><w:t>1,240.00</w:t></w:r></w:p>` +
				`<w:p><w:r><w:t>Acme Corp</w:t><w:br/><w:t>Bengaluru</w:t></w:r></w:p>` +
				docxFooter}},
			want:      "Invoice INV-2024-018\nTotal due\t1,240.00\nAcme Corp\nBengaluru",
			wantPages: 1,
		},
		{
			name: "docx ignores a drawingml t element from another namespace",
			file: "mixed.docx",
			entries: []zipEntry{{"word/document.xml", `<?xml version="1.0"?>` +
				`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"` +
				` xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><w:body>` +
				`<w:p><w:r><w:t>Body text</w:t></w:r><a:t>chart label</a:t></w:p>` +
				docxFooter}},
			want:      "Body text",
			wantPages: 1,
		},
		{
			name: "xlsx resolves shared strings and keeps rows",
			file: "ledger.xlsx",
			entries: []zipEntry{
				{"xl/sharedStrings.xml", `<?xml version="1.0"?>` +
					`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
					`<si><t>Description</t></si><si><t>Amount</t></si>` +
					`<si><r><t>Consulting</t></r><r><t> fee</t></r></si>` +
					`</sst>`},
				{"xl/worksheets/sheet1.xml", sheetHeader +
					`<row><c t="s"><v>0</v></c><c t="s"><v>1</v></c></row>` +
					`<row><c t="s"><v>2</v></c><c><v>1240</v></c></row>` +
					`<row><c t="inlineStr"><is><t>GST 18%</t></is></c><c><v>223.2</v></c></row>` +
					sheetFooter},
			},
			want:      "Description\tAmount\nConsulting fee\t1240\nGST 18%\t223.2",
			wantPages: 1,
		},
		{
			name: "xlsx reads sheet2 before sheet10",
			file: "many.xlsx",
			entries: []zipEntry{
				{"xl/worksheets/sheet10.xml", sheetHeader + `<row><c t="inlineStr"><is><t>tenth</t></is></c></row>` + sheetFooter},
				{"xl/worksheets/sheet2.xml", sheetHeader + `<row><c t="inlineStr"><is><t>second</t></is></c></row>` + sheetFooter},
				{"xl/worksheets/_rels/sheet2.xml.rels", `<Relationships/>`},
			},
			want:      "second\ntenth",
			wantPages: 2,
		},
		{
			name: "xlsx survives an out-of-range shared string index",
			file: "corruptindex.xlsx",
			entries: []zipEntry{
				{"xl/sharedStrings.xml", `<?xml version="1.0"?>` +
					`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>only</t></si></sst>`},
				{"xl/worksheets/sheet1.xml", sheetHeader +
					`<row><c t="s"><v>0</v></c><c t="s"><v>99</v></c></row>` + sheetFooter},
			},
			want:      "only",
			wantPages: 1,
		},
		{
			name: "pptx reads slides in order",
			file: "deck.pptx",
			entries: []zipEntry{
				{"ppt/slides/slide2.xml", slideHeader + `<a:p><a:r><a:t>Second slide</a:t></a:r></a:p>` + slideFooter},
				{"ppt/slides/slide1.xml", slideHeader +
					`<a:p><a:r><a:t>Quarterly</a:t></a:r><a:r><a:t xml:space="preserve"> review</a:t></a:r></a:p>` +
					`<a:p><a:r><a:t>Q3</a:t><a:br/><a:t>FY25</a:t></a:r></a:p>` + slideFooter},
				{"ppt/slides/_rels/slide1.xml.rels", `<Relationships/>`},
			},
			want:      "Quarterly review\nQ3\nFY25\nSecond slide",
			wantPages: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeZipFixture(t, tc.file, tc.entries)
			res, err := (&Office{}).Extract(context.Background(), path)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if res.Text != tc.want {
				t.Errorf("text =\n%q\nwant\n%q", res.Text, tc.want)
			}
			if res.Engine != "office" {
				t.Errorf("engine = %q, want office", res.Engine)
			}
			if res.Pages != tc.wantPages {
				t.Errorf("pages = %d, want %d", res.Pages, tc.wantPages)
			}
		})
	}
}

// TestOfficeRefusesLegacyBinaryFormats: a `.doc` must be told it is a `.doc`,
// not handed a failure that blames the machine for something no machine can fix.
func TestOfficeRefusesLegacyBinaryFormats(t *testing.T) {
	for _, tc := range []struct{ file, product, modern string }{
		{"letter.doc", "Word 97-2003", ".docx"},
		{"budget.XLS", "Excel 97-2003", ".xlsx"},
		{"pitch.ppt", "PowerPoint 97-2003", ".pptx"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			// Real OLE2 magic, so the refusal is about the format and not about
			// the bytes being unreadable.
			path := writeTemp(t, tc.file, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
			_, err := (&Office{}).Extract(context.Background(), path)
			if err == nil {
				t.Fatal("a legacy binary Office file was accepted")
			}
			if !errors.Is(err, ErrLegacyOffice) {
				t.Fatalf("error = %v, want it to wrap ErrLegacyOffice", err)
			}
			msg := err.Error()
			for _, want := range []string{tc.product, tc.modern} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
			if strings.Contains(msg, "machine") || strings.Contains(msg, "install") {
				t.Errorf("the refusal blames the machine or promises a tool: %v", err)
			}
		})
	}
}

// TestOfficeRejectsHostileArchives: everything a user might drop in a folder
// that is not the document it claims to be must produce a clean error and never
// a panic.
func TestOfficeRejectsHostileArchives(t *testing.T) {
	deepXML := docxHeader + strings.Repeat("<w:x>", maxOfficeXMLDepth+16) +
		strings.Repeat("</w:x>", maxOfficeXMLDepth+16) + docxFooter

	tests := []struct {
		name    string
		file    string
		raw     []byte     // written as-is when non-nil
		entries []zipEntry // otherwise zipped
		wantMsg string
	}{
		{
			name:    "empty file",
			file:    "empty.docx",
			raw:     []byte{},
			wantMsg: "not a readable .docx archive",
		},
		{
			name:    "truncated archive",
			file:    "cut.docx",
			raw:     []byte("PK\x03\x04 this stops here"),
			wantMsg: "not a readable .docx archive",
		},
		{
			name:    "zip without the expected part",
			file:    "wrong.docx",
			entries: []zipEntry{{"README.txt", "not a document"}},
			wantMsg: "has no word/document.xml",
		},
		{
			name: "path traversal cannot impersonate the real part",
			file: "evil.docx",
			entries: []zipEntry{{"evil/word/document.xml", docxHeader +
				`<w:p><w:r><w:t>injected</w:t></w:r></w:p>` + docxFooter}},
			wantMsg: "has no word/document.xml",
		},
		{
			name:    "part is not XML",
			file:    "binary.docx",
			entries: []zipEntry{{"word/document.xml", "\x00\x01\x02 not xml at all"}},
			wantMsg: "is not valid XML",
		},
		{
			name:    "deeply nested XML",
			file:    "deep.docx",
			entries: []zipEntry{{"word/document.xml", deepXML}},
			wantMsg: "XML nesting is deeper",
		},
		{
			name: "entity expansion is refused, not expanded",
			file: "laughs.docx",
			entries: []zipEntry{{"word/document.xml", `<?xml version="1.0"?>` +
				`<!DOCTYPE w:document [<!ENTITY lol "haha"><!ENTITY lol2 "&lol;&lol;&lol;">]>` +
				`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
				`<w:p><w:r><w:t>&lol2;</w:t></w:r></w:p>` + docxFooter}},
			wantMsg: "is not valid XML",
		},
		{
			name:    "workbook without worksheets",
			file:    "hollow.xlsx",
			entries: []zipEntry{{"xl/sharedStrings.xml", `<sst/>`}},
			wantMsg: "has no xl/worksheets/",
		},
		{
			name:    "presentation without slides",
			file:    "hollow.pptx",
			entries: []zipEntry{{"ppt/presentation.xml", `<p/>`}},
			wantMsg: "has no ppt/slides/",
		},
		{
			name: "an OOXML file carrying no text",
			file: "blank.docx",
			entries: []zipEntry{{"word/document.xml", docxHeader +
				`<w:p><w:r><w:t>   </w:t></w:r></w:p>` + docxFooter}},
			wantMsg: "carries no text",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.raw != nil {
				path = writeTemp(t, tc.file, tc.raw)
			} else {
				path = writeZipFixture(t, tc.file, tc.entries)
			}
			res, err := (&Office{}).Extract(context.Background(), path)
			if err == nil {
				t.Fatalf("accepted a hostile archive, returning %q", res.Text)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantMsg)
			}
			if res.Engine != "none" {
				t.Errorf("engine = %q on failure, want none", res.Engine)
			}
		})
	}
}

// TestOfficeRefusesTooManyEntries: an archive whose central directory is itself
// the attack is refused before a single entry is decompressed.
func TestOfficeRefusesTooManyEntries(t *testing.T) {
	entries := make([]zipEntry, 0, maxOfficeArchiveEntries+2)
	entries = append(entries, zipEntry{"word/document.xml", docxHeader +
		`<w:p><w:r><w:t>bait</w:t></w:r></w:p>` + docxFooter})
	for i := 0; i < maxOfficeArchiveEntries+1; i++ {
		entries = append(entries, zipEntry{fmt.Sprintf("pad/%d.bin", i), "x"})
	}
	path := writeZipFixture(t, "swarm.docx", entries)

	if _, err := (&Office{}).Extract(context.Background(), path); err == nil {
		t.Fatal("an archive over the entry limit was accepted")
	} else if !strings.Contains(err.Error(), "entry limit") {
		t.Errorf("error = %v, want it to name the entry limit", err)
	}
}

// TestOfficeRefusesDecompressionBomb: the budget is spent against bytes
// actually read, so a header that lies about the uncompressed size buys the
// attacker nothing.
//
// The budget is exercised through officeArchive directly, with a small one, so
// the test proves the mechanism without allocating the production 64 MiB.
func TestOfficeRefusesDecompressionBomb(t *testing.T) {
	const bombBytes = 1 << 20
	path := writeZipFixture(t, "bomb.docx", []zipEntry{
		{"word/document.xml", strings.Repeat("A", bombBytes)},
	})
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > bombBytes/16 {
		t.Fatalf("fixture did not compress; it is %d bytes on disk", info.Size())
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		t.Fatal(err)
	}

	a := &officeArchive{zip: zr, name: filepath.Base(path), budget: 4096}
	if _, err := a.open("word/document.xml"); err == nil {
		t.Fatal("an entry expanding past the budget was read in full")
	} else if !strings.Contains(err.Error(), "decompression budget") {
		t.Errorf("error = %v, want it to name the decompression budget", err)
	}
	if a.budget != 4096 {
		t.Errorf("budget = %d after a refusal, want it unspent", a.budget)
	}
}

// TestOfficeCapsTotalText: a document longer than the output cap is truncated
// rather than returned whole, the same way a plain-text file is.
func TestOfficeCapsTotalText(t *testing.T) {
	var body strings.Builder
	body.WriteString(docxHeader)
	for i := 0; i < 40000; i++ {
		body.WriteString(`<w:p><w:r><w:t>line of ordinary invoice text</w:t></w:r></w:p>`)
	}
	body.WriteString(docxFooter)
	path := writeZipFixture(t, "long.docx", []zipEntry{{"word/document.xml", body.String()}})

	res, err := (&Office{}).Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Text) > MaxOfficeTextBytes {
		t.Errorf("text is %d bytes, over the %d-byte cap", len(res.Text), MaxOfficeTextBytes)
	}
	if len(res.Text) < MaxOfficeTextBytes/2 {
		t.Errorf("text is only %d bytes; the cap should have been reached, not the document", len(res.Text))
	}
}

// TestExtractorRoutesOfficeBeforeOCR: an OOXML file must reach the Office
// runner, and it must get there before the PDF and Vision tiers, which have
// nothing to offer a ZIP of XML.
func TestExtractorRoutesOfficeBeforeOCR(t *testing.T) {
	e := &Extractor{
		Text: &PlainText{}, Office: &Office{}, TextUtil: &TextUtil{}, Legacy: &LegacyOffice{},
		PDF: &PDFToText{}, Vision: &Vision{}, Ollama: &Ollama{Enabled: "false"},
	}

	docx := writeZipFixture(t, "routed.docx", []zipEntry{{"word/document.xml", docxHeader +
		`<w:p><w:r><w:t>Statement of account</w:t></w:r></w:p>` + docxFooter}})
	res, err := e.Extract(context.Background(), docx)
	if err != nil {
		t.Fatalf("Extract(.docx): %v", err)
	}
	if res.Engine != "office" || res.Text != "Statement of account" {
		t.Errorf("engine = %q text = %q, want office and the document's text", res.Engine, res.Text)
	}

	// A legacy file forced through this runner is refused by name rather than
	// mangled: Office reads ZIPs of XML, and an OLE2 compound file is neither.
	forced := &Extractor{engine: "office", Office: &Office{}}
	doc := writeTemp(t, "routed.doc", []byte{0xD0, 0xCF, 0x11, 0xE0})
	if _, err := forced.Extract(context.Background(), doc); !errors.Is(err, ErrLegacyOffice) {
		t.Errorf("Extract(.doc) with --engine office = %v, want the legacy-format refusal", err)
	}
}

// TestOfficeDoctorLine: doctor must show the runner as always available, since
// it needs no external tool.
func TestOfficeDoctorLine(t *testing.T) {
	o := &Office{}
	if !o.Available() {
		t.Fatal("the Office runner reported itself unavailable")
	}
	detail := o.detail()
	for _, want := range []string{".docx", ".xlsx", ".pptx", "no external tool", ".doc"} {
		if !strings.Contains(detail, want) {
			t.Errorf("doctor detail %q does not mention %q", detail, want)
		}
	}
}
