package ocr

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// MaxOfficeTextBytes caps how much text one Office document may yield.
//
// It matches MaxPlainTextBytes deliberately: both are read budgets, both are
// far larger than sidecar.MaxText (256 KiB) and classify's window, and a
// document that needs more than a megabyte of leading text to be classifiable
// is not one more bytes would help. Text past the cap is dropped silently, the
// same way a plain-text file past its cap is.
const MaxOfficeTextBytes = MaxPlainTextBytes

// maxOfficeArchiveBytes caps the total *decompressed* bytes read out of one
// archive, across every entry.
//
// A ZIP is attacker-controlled input and its headers are attacker-controlled
// too, so the declared uncompressed size is never consulted: this budget is
// spent against bytes actually read through an io.LimitReader. 64 MiB is far
// above any real document's XML and far below what a zip bomb wants.
const maxOfficeArchiveBytes = 64 << 20

// maxOfficeArchiveEntries caps how many members an archive may declare. A real
// Office file has tens; a bomb declares millions to make the central directory
// itself the attack.
const maxOfficeArchiveEntries = 8192

// maxOfficeXMLDepth caps element nesting. Real OOXML nests a few dozen deep;
// anything past this is a file built to exhaust the decoder, and it earns a
// clean error rather than an unbounded walk.
const maxOfficeXMLDepth = 4096

// OOXML namespaces. Matching on the namespace as well as the local name keeps
// `w:t` (Word text) apart from `a:t` (DrawingML text) inside the same part.
const (
	nsWordprocessing = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsSpreadsheet    = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsDrawing        = "http://schemas.openxmlformats.org/drawingml/2006/main"
)

// OfficeExtensions are the Office formats Kagaz reads, lowercased and including
// the dot. They are all OOXML: a ZIP archive of XML parts, which the standard
// library opens without any third-party package.
var OfficeExtensions = []string{".docx", ".xlsx", ".pptx"}

// LegacyOfficeExtensions are the pre-2007 binary Office formats. This runner
// does not read them -- they are OLE2 compound files, not ZIPs -- but Kagaz
// does: `.doc` through the TextUtil tier and `.xls`/`.ppt` through LegacyOffice.
// The list stays here so the extension set has one home.
var LegacyOfficeExtensions = []string{".doc", ".xls", ".ppt"}

// ErrLegacyOffice means the file is a pre-2007 binary Office document (an OLE2
// compound file), which Kagaz does not parse.
var ErrLegacyOffice = errors.New("legacy binary Office format is not supported")

// legacyOfficeNames gives each legacy extension the name a user would recognise
// on the Save-As menu, so the refusal names a format rather than a mystery.
var legacyOfficeNames = map[string]struct{ product, modern string }{
	".doc": {"Word 97-2003", ".docx"},
	".xls": {"Excel 97-2003", ".xlsx"},
	".ppt": {"PowerPoint 97-2003", ".pptx"},
}

// Office extracts text from OOXML documents -- `.docx`, `.xlsx`, `.pptx` --
// by reading the archive's XML parts directly.
//
// It exists because a dry run over a real 750-file folder found 76 `.docx` and
// 21 `.xlsx` files that Kagaz could not read at all: no PDF text layer to find
// and nothing OCR could do, since there is no image. The text was there the
// whole time, one `archive/zip` call away.
//
// This runner is always available: like PlainText it depends on nothing but the
// standard library, so it is a tier that cannot degrade (Global Constraint 9
// has nothing to do here).
//
// OpenDocument (`.odt`/`.ods`/`.odp`) is deliberately *not* handled. Its text
// model is a second decoder rather than a free ride -- different namespaces,
// its own tab and line-break elements, and paragraph semantics that differ per
// body type -- and nothing in the dry run needed it.
type Office struct{}

// Name identifies the runner in Result.Engine and doctor output.
func (o *Office) Name() string { return "office" }

// Available is always true: reading a ZIP of XML needs no external tool.
func (o *Office) Available() bool { return true }

// Handles reports whether path is an OOXML document this runner can read.
func (o *Office) Handles(path string) bool {
	return extIn(path, OfficeExtensions)
}

// HandlesLegacy reports whether path is a pre-2007 binary Office document.
// This runner does not read those -- the textutil and legacyoffice tiers do --
// and says so by name rather than letting the file fall through to OCR and fail
// as a generic "no text".
func (o *Office) HandlesLegacy(path string) bool {
	return extIn(path, LegacyOfficeExtensions)
}

// extIn reports whether path's lowercased extension is in list.
func extIn(path string, list []string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, want := range list {
		if ext == want {
			return true
		}
	}
	return false
}

// Extract reads the document's text.
//
// Every failure here is a clean error, never a panic: the input is whatever the
// user dropped in a folder, so a truncated archive, a missing part, an entry
// that is not XML and an archive built to expand forever are all ordinary
// outcomes rather than exceptional ones.
func (o *Office) Extract(_ context.Context, path string) (Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if legacy, ok := legacyOfficeNames[ext]; ok {
		// Name the format and say what actually works. This is reachable only
		// when a caller forces --engine office; automatic selection sends these
		// to the tiers that do read them. Either way the answer is about the
		// format, never about the machine having a bad day.
		return Result{Engine: "none"}, fmt.Errorf(
			"office: %s: %w by this runner -- a %s document (%s) is an OLE2 compound file, not a ZIP of XML; "+
				"let kagaz choose the extractor (the textutil and legacyoffice tiers read these), or re-save it as %s",
			filepath.Base(path), ErrLegacyOffice, legacy.product, ext, legacy.modern)
	}

	zr, err := zip.OpenReader(path)
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("office: %s: not a readable %s archive: %w",
			filepath.Base(path), ext, err)
	}
	defer func() { _ = zr.Close() }()

	if len(zr.File) > maxOfficeArchiveEntries {
		return Result{Engine: "none"}, fmt.Errorf(
			"office: %s: the archive declares %d entries, over the %d-entry limit; it is not a document",
			filepath.Base(path), len(zr.File), maxOfficeArchiveEntries)
	}

	a := &officeArchive{zip: &zr.Reader, budget: maxOfficeArchiveBytes, name: filepath.Base(path)}
	sink := &textSink{limit: MaxOfficeTextBytes}

	var pages int
	switch ext {
	case ".docx":
		pages, err = extractDocx(a, sink)
	case ".xlsx":
		pages, err = extractXlsx(a, sink)
	case ".pptx":
		pages, err = extractPptx(a, sink)
	default:
		return Result{Engine: "none"}, fmt.Errorf("office: %s: %s is not an Office format this runner reads",
			filepath.Base(path), ext)
	}
	if err != nil {
		return Result{Engine: "none"}, err
	}

	text := strings.TrimSpace(sink.String())
	if text == "" {
		return Result{Engine: "none"}, fmt.Errorf("office: %s: %w (the document carries no text)",
			filepath.Base(path), ErrNoText)
	}
	if pages < 1 {
		pages = 1
	}
	return Result{Text: text, Engine: o.Name(), Confidence: 1, Pages: pages}, nil
}

// detail is the doctor line for this runner.
func (o *Office) detail() string {
	return "built in; reads " + strings.Join(OfficeExtensions, ", ") +
		" with no external tool (" + strings.Join(LegacyOfficeExtensions, ", ") +
		" are handled by the textutil and legacyoffice tiers)"
}

// officeArchive reads named parts out of one archive against a shared
// decompression budget.
type officeArchive struct {
	zip    *zip.Reader
	name   string
	budget int64
}

// open returns the bytes of the entry named exactly name.
//
// Exactly: a suffix match would let `evil/word/document.xml` stand in for
// `word/document.xml`, which is the ZIP form of path traversal even though
// nothing is written to disk.
func (a *officeArchive) open(name string) ([]byte, error) {
	for _, f := range a.zip.File {
		if f.Name == name {
			return a.read(f)
		}
	}
	return nil, fmt.Errorf("office: %s: the archive has no %s, so it is not the document its extension claims",
		a.name, name)
}

// read decompresses one entry, spending the archive's shared budget against
// bytes actually read rather than against the header's claim about them.
func (a *officeArchive) read(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("office: %s: %s is corrupt: %w", a.name, f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	// One byte past the budget is enough to prove it was exceeded, and is the
	// most that can be read past it.
	data, err := io.ReadAll(io.LimitReader(rc, a.budget+1))
	if err != nil {
		return nil, fmt.Errorf("office: %s: %s is corrupt: %w", a.name, f.Name, err)
	}
	if int64(len(data)) > a.budget {
		return nil, fmt.Errorf(
			"office: %s: %s expands past the %d MiB decompression budget for one document; it is not a document",
			a.name, f.Name, maxOfficeArchiveBytes>>20)
	}
	a.budget -= int64(len(data))
	return data, nil
}

// parts returns every entry directly inside dir (no deeper) whose name ends in
// suffix, ordered the way a reader would meet them: sheet2 before sheet10.
func (a *officeArchive) parts(dir, suffix string) []*zip.File {
	var out []*zip.File
	for _, f := range a.zip.File {
		if !strings.HasPrefix(f.Name, dir) || !strings.HasSuffix(f.Name, suffix) {
			continue
		}
		if strings.Contains(f.Name[len(dir):], "/") {
			continue // a nested part such as ppt/slides/_rels/slide1.xml.rels
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return partLess(out[i].Name, out[j].Name) })
	return out
}

// partLess orders `sheet2.xml` before `sheet10.xml`, which a plain string sort
// gets backwards. Order is not cosmetic here: it is the order a classifier sees
// the document's own words in.
func partLess(a, b string) bool {
	an, aok := partIndex(a)
	bn, bok := partIndex(b)
	if aok && bok && an != bn {
		return an < bn
	}
	if aok != bok {
		return aok
	}
	return a < b
}

// partIndex pulls the trailing number out of a part name such as
// "xl/worksheets/sheet12.xml".
func partIndex(name string) (int, bool) {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	i := len(base)
	for i > 0 && base[i-1] >= '0' && base[i-1] <= '9' {
		i--
	}
	if i == len(base) {
		return 0, false
	}
	n, err := strconv.Atoi(base[i:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// textSink accumulates extracted text up to a hard byte cap.
type textSink struct {
	b         strings.Builder
	limit     int
	truncated bool
}

// write appends s, stopping at the cap. Once full the sink stays full, and
// callers stop walking rather than parsing a megabyte they will throw away.
func (s *textSink) write(str string) {
	if s.truncated {
		return
	}
	if s.b.Len()+len(str) > s.limit {
		s.b.WriteString(str[:s.limit-s.b.Len()])
		s.truncated = true
		return
	}
	s.b.WriteString(str)
}

// full reports whether the cap has been reached.
func (s *textSink) full() bool { return s.truncated }

// line writes one logical line -- a paragraph, a row, a slide's text -- unless
// it is blank.
func (s *textSink) line(str string) {
	if strings.TrimSpace(str) == "" {
		return
	}
	s.write(str)
	s.write("\n")
}

// String is everything written so far.
func (s *textSink) String() string { return s.b.String() }

// newOfficeDecoder decodes one XML part.
//
// It sets nothing about entities on purpose. encoding/xml never resolves
// external entities -- it has no I/O to do so -- and with Strict left true an
// undeclared entity reference is a decode error rather than an expansion, so
// the billion-laughs shape cannot inflate here. Decoder.Entity, which is the
// only way to teach it any entity beyond the five XML predefines, is left nil.
func newOfficeDecoder(data []byte) *xml.Decoder {
	return xml.NewDecoder(bytes.NewReader(data))
}

// elementText returns the character data directly inside the element whose
// start tag was just read, consuming through its end tag.
func elementText(dec *xml.Decoder) (string, error) {
	var b strings.Builder
	depth := 1
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if depth == 1 {
				b.Write(t)
			}
		case xml.StartElement:
			depth++
			if depth > maxOfficeXMLDepth {
				return "", errXMLTooDeep
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				return b.String(), nil
			}
		}
	}
}

// errXMLTooDeep is the nesting refusal, wrapped with the part name by callers.
var errXMLTooDeep = errors.New("XML nesting is deeper than any document needs")

// xmlErr turns a decode failure into an error that names the part.
func xmlErr(name, part string, err error) error {
	if errors.Is(err, errXMLTooDeep) {
		return fmt.Errorf("office: %s: %s: %w", name, part, err)
	}
	return fmt.Errorf("office: %s: %s is not valid XML: %w", name, part, err)
}

// extractDocx reads word/document.xml.
//
// Paragraph boundaries are honoured because `<w:t>` runs split mid-sentence at
// every formatting change: concatenating them without the `<w:p>` breaks runs
// the last word of one paragraph into the first word of the next, and a
// classifier reading "TotalDue" learns nothing.
func extractDocx(a *officeArchive, sink *textSink) (int, error) {
	const part = "word/document.xml"
	data, err := a.open(part)
	if err != nil {
		return 0, err
	}
	dec := newOfficeDecoder(data)

	var para strings.Builder
	depth := 0
	for !sink.full() {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, xmlErr(a.name, part, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxOfficeXMLDepth {
				return 0, xmlErr(a.name, part, errXMLTooDeep)
			}
			if t.Name.Space != nsWordprocessing {
				continue
			}
			switch t.Name.Local {
			case "t":
				s, err := elementText(dec)
				if err != nil {
					return 0, xmlErr(a.name, part, err)
				}
				depth--
				para.WriteString(s)
			case "tab":
				para.WriteString("\t")
			case "br", "cr":
				para.WriteString("\n")
			}
		case xml.EndElement:
			depth--
			if t.Name.Space == nsWordprocessing && t.Name.Local == "p" {
				sink.line(para.String())
				para.Reset()
			}
		}
	}
	sink.line(para.String())
	return 1, nil
}

// extractXlsx reads the shared string table and then every worksheet.
//
// Cells are emitted row by row, tab-separated. A spreadsheet's value to a
// classifier is its labels and its numbers, not its column widths, so no
// attempt is made to reproduce the grid: empty cells are skipped rather than
// padded, which keeps a mostly-empty sheet from drowning its own text in tabs.
func extractXlsx(a *officeArchive, sink *textSink) (int, error) {
	shared, err := sharedStrings(a)
	if err != nil {
		return 0, err
	}
	sheets := a.parts("xl/worksheets/", ".xml")
	if len(sheets) == 0 {
		return 0, fmt.Errorf("office: %s: the archive has no xl/worksheets/, so it is not a workbook", a.name)
	}
	for _, f := range sheets {
		if sink.full() {
			break
		}
		data, err := a.read(f)
		if err != nil {
			return 0, err
		}
		if err := extractSheet(a.name, f.Name, data, shared, sink); err != nil {
			return 0, err
		}
	}
	return len(sheets), nil
}

// sharedStrings reads xl/sharedStrings.xml, the table most cell values point
// into. Its absence is normal -- a workbook of only numbers has none -- so a
// missing part yields an empty table rather than an error.
func sharedStrings(a *officeArchive) ([]string, error) {
	const part = "xl/sharedStrings.xml"
	var file *zip.File
	for _, f := range a.zip.File {
		if f.Name == part {
			file = f
			break
		}
	}
	if file == nil {
		return nil, nil
	}
	data, err := a.read(file)
	if err != nil {
		return nil, err
	}

	dec := newOfficeDecoder(data)
	var out []string
	var cur strings.Builder
	inItem := false
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, xmlErr(a.name, part, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxOfficeXMLDepth {
				return nil, xmlErr(a.name, part, errXMLTooDeep)
			}
			if t.Name.Space != nsSpreadsheet {
				continue
			}
			switch t.Name.Local {
			case "si":
				inItem, cur = true, strings.Builder{}
			case "t":
				s, err := elementText(dec)
				if err != nil {
					return nil, xmlErr(a.name, part, err)
				}
				depth--
				if inItem {
					// A rich-text string is several <t> runs; they are one value.
					cur.WriteString(s)
				}
			}
		case xml.EndElement:
			depth--
			if t.Name.Space == nsSpreadsheet && t.Name.Local == "si" {
				out = append(out, cur.String())
				inItem = false
			}
		}
		if len(out) > maxOfficeArchiveEntries*64 {
			return nil, fmt.Errorf("office: %s: %s declares an implausible number of strings", a.name, part)
		}
	}
	return out, nil
}

// extractSheet walks one worksheet part, resolving each cell against the shared
// string table.
func extractSheet(name, part string, data []byte, shared []string, sink *textSink) error {
	dec := newOfficeDecoder(data)

	var row []string
	var cellType string
	depth := 0
	for !sink.full() {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return xmlErr(name, part, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxOfficeXMLDepth {
				return xmlErr(name, part, errXMLTooDeep)
			}
			if t.Name.Space != nsSpreadsheet {
				continue
			}
			switch t.Name.Local {
			case "c":
				cellType = ""
				for _, at := range t.Attr {
					if at.Name.Local == "t" {
						cellType = at.Value
					}
				}
			case "v":
				s, err := elementText(dec)
				if err != nil {
					return xmlErr(name, part, err)
				}
				depth--
				if cellType == "s" {
					// An index into the shared table. An out-of-range index is
					// a corrupt workbook, not a reason to fail the document.
					if i, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && i >= 0 && i < len(shared) {
						s = shared[i]
					} else {
						s = ""
					}
				}
				if strings.TrimSpace(s) != "" {
					row = append(row, s)
				}
			case "t":
				// An inline string: <c t="inlineStr"><is><t>text</t></is></c>.
				s, err := elementText(dec)
				if err != nil {
					return xmlErr(name, part, err)
				}
				depth--
				if strings.TrimSpace(s) != "" {
					row = append(row, s)
				}
			}
		case xml.EndElement:
			depth--
			if t.Name.Space == nsSpreadsheet && t.Name.Local == "row" {
				sink.line(strings.Join(row, "\t"))
				row = row[:0]
			}
		}
	}
	sink.line(strings.Join(row, "\t"))
	return nil
}

// extractPptx reads ppt/slides/slideN.xml in slide order.
func extractPptx(a *officeArchive, sink *textSink) (int, error) {
	slides := a.parts("ppt/slides/", ".xml")
	if len(slides) == 0 {
		return 0, fmt.Errorf("office: %s: the archive has no ppt/slides/, so it is not a presentation", a.name)
	}
	for _, f := range slides {
		if sink.full() {
			break
		}
		data, err := a.read(f)
		if err != nil {
			return 0, err
		}
		if err := extractSlide(a.name, f.Name, data, sink); err != nil {
			return 0, err
		}
	}
	return len(slides), nil
}

// extractSlide walks one slide part, one DrawingML paragraph per line.
func extractSlide(name, part string, data []byte, sink *textSink) error {
	dec := newOfficeDecoder(data)

	var para strings.Builder
	depth := 0
	for !sink.full() {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return xmlErr(name, part, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxOfficeXMLDepth {
				return xmlErr(name, part, errXMLTooDeep)
			}
			if t.Name.Space != nsDrawing {
				continue
			}
			switch t.Name.Local {
			case "t":
				s, err := elementText(dec)
				if err != nil {
					return xmlErr(name, part, err)
				}
				depth--
				para.WriteString(s)
			case "br":
				para.WriteString("\n")
			}
		case xml.EndElement:
			depth--
			if t.Name.Space == nsDrawing && t.Name.Local == "p" {
				sink.line(para.String())
				para.Reset()
			}
		}
	}
	sink.line(para.String())
	return nil
}
