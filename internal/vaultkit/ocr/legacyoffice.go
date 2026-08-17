package ocr

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// maxBIFFRecords bounds the record walk. A real workbook has thousands; the cap
// is what stops a file whose records are all two bytes long from turning a
// megabyte of stream into an unbounded loop.
const maxBIFFRecords = 1 << 20

// maxSSTStrings bounds the shared string table. The count is a 32-bit field
// read straight out of the file, so it is a hint to be checked, never a size to
// allocate from.
const maxSSTStrings = 1 << 20

// maxSSTStringChars bounds one string's declared character count. BIFF8 caps a
// cell string at 32767 characters; anything longer is a corrupt or hostile
// length field.
const maxSSTStringChars = 32767

// BIFF record types. Only the ones that carry text or numbers are listed: the
// point is a classifier's view of the document, not a reconstruction of it.
const (
	biffCONTINUE  = 0x003C
	biffSST       = 0x00FC
	biffLABELSST  = 0x00FD
	biffLABEL     = 0x0204
	biffRK        = 0x027E
	biffMULRK     = 0x00BD
	biffNUMBER    = 0x0203
	biffBOUNDSHET = 0x0085
	biffFILEPASS  = 0x002F
	biffEOF       = 0x000A
)

// PowerPoint record types that carry slide text.
const (
	pptTextCharsAtom = 0x0FA0 // UTF-16LE
	pptTextBytesAtom = 0x0FA8 // CP1252-ish, high byte implied zero
	pptCStringAtom   = 0x0FBA // UTF-16LE, used for titles and notes headers
)

// maxPPTDepth bounds container recursion in a PowerPoint stream. Real files
// nest a handful deep; a file that nests thousands deep is built to blow the
// stack, and it earns a clean error instead.
const maxPPTDepth = 64

// LegacyOffice extracts text from the pre-2007 binary Office formats that no
// system tool on macOS reads: `.xls` and `.ppt`.
//
// Both are OLE2 compound files, so both go through one reader (cfb.go) and
// differ only in which stream is walked afterwards. A dry run over a real
// 750-file folder found 21 `.xls` files that Kagaz could not read at all --
// there is no PDF text layer to find and no image for OCR, so without this tier
// they are simply invisible.
//
// What comes out is deliberately not a spreadsheet. A workbook's value to a
// classifier is its labels and its numbers; column widths, formulas and merged
// cells are noise, so cells are emitted row by row and tab-separated with empty
// cells skipped rather than padded.
//
// This runner is always available: it depends on nothing but the standard
// library, so it is a tier that cannot degrade (Global Constraint 9 has nothing
// to do here). Every failure is a clean error and never a panic -- the input is
// whatever a user dropped in a folder, so a truncated container, a cyclic
// sector chain and a length field claiming gigabytes are ordinary outcomes.
type LegacyOffice struct{}

// Name identifies the runner in Result.Engine and doctor output.
func (l *LegacyOffice) Name() string { return "legacyoffice" }

// Available is always true: reading a compound file needs no external tool.
func (l *LegacyOffice) Available() bool { return true }

// Handles reports whether path is a legacy binary Office file this runner
// parses.
func (l *LegacyOffice) Handles(path string) bool {
	return extIn(path, CompoundOfficeExtensions)
}

// Extract reads path's text.
func (l *LegacyOffice) Extract(ctx context.Context, path string) (Result, error) {
	ext := strings.ToLower(filepath.Ext(path))
	base := filepath.Base(path)

	data, err := readCapped(path, maxCFBBytes)
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("legacyoffice: %s: %w", base, err)
	}

	doc, err := openCFB(data)
	if err != nil {
		if errors.Is(err, ErrNotCFB) {
			return Result{Engine: "none"}, fmt.Errorf(
				"legacyoffice: %s: %w -- %s is an OLE2 compound file and this is not one",
				base, err, officeFormatNoun(ext))
		}
		return Result{Engine: "none"}, fmt.Errorf("legacyoffice: %s: %w", base, err)
	}

	sink := &textSink{limit: MaxOfficeTextBytes, ctx: ctx}
	var pages int
	switch ext {
	case ".xls":
		pages, err = extractXLS(doc, sink)
	case ".ppt":
		pages, err = extractPPT(doc, sink)
	default:
		return Result{Engine: "none"}, fmt.Errorf(
			"legacyoffice: %s: %s is not a legacy Office format this runner reads", base, ext)
	}
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("legacyoffice: %s: %w", base, err)
	}
	if err := sink.err(); err != nil {
		return Result{Engine: "none"}, fmt.Errorf("legacyoffice: %s: %w", base, err)
	}

	text := strings.TrimSpace(sink.String())
	if text == "" {
		return Result{Engine: "none"}, fmt.Errorf("legacyoffice: %s: %w (%s carries no text)",
			base, ErrNoText, officeFormatNoun(ext))
	}
	if pages < 1 {
		pages = 1
	}
	return Result{Text: text, Engine: l.Name(), Confidence: 1, Pages: pages}, nil
}

// detail is the doctor line for this runner.
func (l *LegacyOffice) detail() string {
	return "built in; reads " + strings.Join(CompoundOfficeExtensions, ", ") +
		" (OLE2 compound files) with no external tool"
}

// readCapped reads at most max bytes of path, and refuses a file that is
// larger rather than returning its prefix.
//
// The prefix is the trap: a legitimate 80 MB workbook truncated at 64 MiB
// parses as a compound file whose sector chains run off the end, so the user is
// told their file is corrupt when it is merely bigger than Kagaz reads. Those
// are different problems with different answers, and only one of them is true.
func readCapped(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// One byte past the cap is enough to prove the file exceeds it.
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, fmt.Errorf(
			"the file is larger than the %d MiB this tier reads, so it was not parsed (it is not corrupt: it is too big)",
			max>>20)
	}
	return data, nil
}

// biffRecord is one BIFF record's type and payload. The payload is a subslice
// of the stream, already bounds-checked.
type biffRecord struct {
	Type uint16
	Data []byte
}

// walkBIFF splits a workbook stream into records.
//
// A record header that runs past the end of the stream stops the walk rather
// than failing the document: legacy files truncated by old sync tools are
// common, and the records that did survive still classify. A header that is
// impossible -- claiming a length the stream cannot hold when there is more
// stream after it -- stops the walk for the same reason.
func walkBIFF(stream []byte) []biffRecord {
	out := make([]biffRecord, 0, 256)
	for pos := 0; pos+4 <= len(stream) && len(out) < maxBIFFRecords; {
		typ := binary.LittleEndian.Uint16(stream[pos : pos+2])
		size := int(binary.LittleEndian.Uint16(stream[pos+2 : pos+4]))
		pos += 4
		if pos+size > len(stream) {
			// Keep what is readable of the last record rather than dropping it.
			size = len(stream) - pos
			if size > 0 {
				out = append(out, biffRecord{Type: typ, Data: stream[pos : pos+size]})
			}
			break
		}
		out = append(out, biffRecord{Type: typ, Data: stream[pos : pos+size]})
		pos += size
	}
	return out
}

// extractXLS reads the workbook stream and emits its labels and numbers.
func extractXLS(doc *cfbFile, sink *textSink) (int, error) {
	name := "Workbook"
	if !doc.hasStream(name) {
		// BIFF5/BIFF7 (Excel 5.0/95) call it "Book". The record types this
		// reader wants are BIFF8's, so a BIFF5 file mostly yields nothing --
		// but LABEL and NUMBER are shared, so it is worth the try.
		name = "Book"
	}
	stream, err := doc.stream(name)
	if err != nil {
		return 0, err
	}

	records := walkBIFF(stream)
	for _, r := range records {
		if r.Type == biffFILEPASS {
			return 0, errors.New("the workbook is password-protected, so its text cannot be read")
		}
	}

	sst := readSST(records)

	// Sheet names are the cheapest real labels in the file: "Invoices Q3"
	// tells a classifier as much as a column heading does.
	for _, r := range records {
		if r.Type != biffBOUNDSHET || sink.full() {
			continue
		}
		if s, ok := boundSheetName(r.Data); ok {
			sink.line("Sheet: " + s)
		}
	}

	// Cells are collected per row so a row comes out as a row. Rows are kept in
	// a map because BIFF does not guarantee row order and a classifier reading
	// a jumbled sheet loses the label/value pairing that makes it legible.
	//
	// Accumulation is bounded by the sink, not by the file. The guard that used
	// to stand here counted rows against maxBIFFRecords, which is dead code: the
	// row key is a uint16, so the map can never hold more than 65536 rows while
	// the cap is 1<<20. Meanwhile a 64 MiB workbook of MULRK records
	// materialised some eleven million cells -- about 600 MB -- to emit at most
	// the one megabyte the sink will take. Each cell is charged its own text
	// plus the one separator byte it would cost to emit, so the accumulation is
	// bounded by the same budget as the output.
	rows := map[uint16][]cellValue{}
	budget := sink.remaining()
	spent := 0
	sheets := 0
	for _, r := range records {
		if r.Type == biffBOUNDSHET {
			sheets++
			continue
		}
		if spent >= budget {
			continue
		}
		row, cells, ok := decodeCells(r, sst)
		if !ok {
			continue
		}
		for _, c := range cells {
			spent += len(c.text) + 1
		}
		rows[row] = append(rows[row], cells...)
	}

	order := make([]uint16, 0, len(rows))
	for row := range rows {
		order = append(order, row)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	for _, row := range order {
		if sink.full() {
			break
		}
		cells := rows[row]
		sort.SliceStable(cells, func(i, j int) bool { return cells[i].col < cells[j].col })
		parts := make([]string, 0, len(cells))
		for _, c := range cells {
			if strings.TrimSpace(c.text) != "" {
				parts = append(parts, c.text)
			}
		}
		sink.line(strings.Join(parts, "\t"))
	}
	return sheets, nil
}

// cellValue is one cell's column and rendered text.
type cellValue struct {
	col  uint16
	text string
}

// decodeCells turns one record into the cells it carries, if it carries any.
//
// Every field is read only after the payload is proved long enough for it: a
// short record is a corrupt record, not a panic.
func decodeCells(r biffRecord, sst []string) (uint16, []cellValue, bool) {
	d := r.Data
	switch r.Type {
	case biffLABELSST:
		if len(d) < 10 {
			return 0, nil, false
		}
		idx := binary.LittleEndian.Uint32(d[6:10])
		if int(idx) >= len(sst) {
			return 0, nil, false
		}
		return binary.LittleEndian.Uint16(d[0:2]),
			[]cellValue{{col: binary.LittleEndian.Uint16(d[2:4]), text: sst[idx]}}, true

	case biffLABEL:
		if len(d) < 8 {
			return 0, nil, false
		}
		s, ok := readXLUnicodeString(d[6:])
		if !ok {
			return 0, nil, false
		}
		return binary.LittleEndian.Uint16(d[0:2]),
			[]cellValue{{col: binary.LittleEndian.Uint16(d[2:4]), text: s}}, true

	case biffRK:
		if len(d) < 10 {
			return 0, nil, false
		}
		return binary.LittleEndian.Uint16(d[0:2]),
			[]cellValue{{
				col:  binary.LittleEndian.Uint16(d[2:4]),
				text: formatNumber(decodeRK(binary.LittleEndian.Uint32(d[6:10]))),
			}}, true

	case biffMULRK:
		// row(2) colFirst(2) then N * (ixfe(2) rk(4)) then colLast(2).
		if len(d) < 6 {
			return 0, nil, false
		}
		row := binary.LittleEndian.Uint16(d[0:2])
		col := binary.LittleEndian.Uint16(d[2:4])
		body := d[4 : len(d)-2]
		var out []cellValue
		for i := 0; i+6 <= len(body); i += 6 {
			out = append(out, cellValue{
				col:  col,
				text: formatNumber(decodeRK(binary.LittleEndian.Uint32(body[i+2 : i+6]))),
			})
			col++
		}
		return row, out, len(out) > 0

	case biffNUMBER:
		if len(d) < 14 {
			return 0, nil, false
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(d[6:14]))
		return binary.LittleEndian.Uint16(d[0:2]),
			[]cellValue{{col: binary.LittleEndian.Uint16(d[2:4]), text: formatNumber(v)}}, true
	}
	return 0, nil, false
}

// decodeRK expands the RK compression: two bits say whether the value is a
// 30-bit signed integer or the top 30 bits of a float64, and whether it was
// divided by 100 on the way in.
func decodeRK(rk uint32) float64 {
	var v float64
	if rk&0x02 != 0 {
		v = float64(int32(rk) >> 2)
	} else {
		v = math.Float64frombits(uint64(rk&0xFFFFFFFC) << 32)
	}
	if rk&0x01 != 0 {
		v /= 100
	}
	return v
}

// formatNumber renders a cell number the way it would be read aloud: no
// exponent for ordinary magnitudes and no trailing zeros.
func formatNumber(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// boundSheetName reads a BOUNDSHEET record's sheet name, which is a
// ShortXLUnicodeString: a one-byte character count, a flags byte, then the
// characters.
func boundSheetName(d []byte) (string, bool) {
	if len(d) < 8 {
		return "", false
	}
	cch := int(d[6])
	flags := d[7]
	body := d[8:]
	if flags&0x01 != 0 {
		if len(body) < cch*2 {
			return "", false
		}
		return decodeUTF16LE(body[:cch*2]), true
	}
	if len(body) < cch {
		return "", false
	}
	return decodeCP1252(body[:cch]), true
}

// readXLUnicodeString reads a 16-bit-counted string: cch(2), flags(1), then
// cch characters of one or two bytes each.
func readXLUnicodeString(d []byte) (string, bool) {
	if len(d) < 3 {
		return "", false
	}
	cch := int(binary.LittleEndian.Uint16(d[0:2]))
	if cch > maxSSTStringChars {
		return "", false
	}
	flags := d[2]
	body := d[3:]
	if flags&0x01 != 0 {
		if len(body) < cch*2 {
			return "", false
		}
		return decodeUTF16LE(body[:cch*2]), true
	}
	if len(body) < cch {
		return "", false
	}
	return decodeCP1252(body[:cch]), true
}

// sstReader reads across an SST record and its CONTINUE records as one logical
// byte stream, while still knowing where the record boundaries are.
//
// The boundaries matter: BIFF8 splits a long string across records at a
// character boundary and restarts the continuation with a fresh flags byte, so
// the second half of a string can be a different width from the first. A reader
// that concatenated the payloads and forgot the seams would decode the tail of
// every long string as garbage.
type sstReader struct {
	blocks [][]byte
	bi     int
	off    int
}

// atEnd reports whether every block is consumed.
func (r *sstReader) atEnd() bool {
	r.settle()
	return r.bi >= len(r.blocks)
}

// settle advances past any exhausted blocks.
func (r *sstReader) settle() {
	for r.bi < len(r.blocks) && r.off >= len(r.blocks[r.bi]) {
		r.bi++
		r.off = 0
	}
}

// atBlockStart reports whether the next byte begins a *continuation* record,
// which is where the fresh flags byte lives. The first block does not count:
// its flags byte was already read as part of the string header.
func (r *sstReader) atBlockStart() bool {
	r.settle()
	return r.bi > 0 && r.off == 0
}

// byteAt reads one byte, crossing record boundaries.
func (r *sstReader) byteAt() (byte, bool) {
	r.settle()
	if r.bi >= len(r.blocks) {
		return 0, false
	}
	b := r.blocks[r.bi][r.off]
	r.off++
	return b, true
}

// remaining is how many bytes are left across every block.
//
// It exists so nothing is ever allocated from a length field before that length
// is checked against the bytes that actually exist: an SST string can declare a
// megabyte-long phonetic block, and sizing a buffer from the claim turns a
// 5 KiB file into gigabytes of allocation churn.
func (r *sstReader) remaining() int {
	r.settle()
	n := 0
	for i := r.bi; i < len(r.blocks); i++ {
		if i == r.bi {
			n += len(r.blocks[i]) - r.off
			continue
		}
		n += len(r.blocks[i])
	}
	return n
}

// take reads n bytes, which may span records.
func (r *sstReader) take(n int) ([]byte, bool) {
	if n > r.remaining() {
		return nil, false
	}
	out := make([]byte, 0, n)
	for len(out) < n {
		r.settle()
		if r.bi >= len(r.blocks) {
			return nil, false
		}
		blk := r.blocks[r.bi]
		take := len(blk) - r.off
		if take > n-len(out) {
			take = n - len(out)
		}
		out = append(out, blk[r.off:r.off+take]...)
		r.off += take
	}
	return out, true
}

// u16 and u32 read little-endian integers across record boundaries.
func (r *sstReader) u16() (uint16, bool) {
	b, ok := r.take(2)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b), true
}

func (r *sstReader) u32() (uint32, bool) {
	b, ok := r.take(4)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b), true
}

// skip discards n bytes without copying them.
func (r *sstReader) skip(n int) bool {
	if n > r.remaining() {
		return false
	}
	for n > 0 {
		r.settle()
		if r.bi >= len(r.blocks) {
			return false
		}
		take := len(r.blocks[r.bi]) - r.off
		if take > n {
			take = n
		}
		r.off += take
		n -= take
	}
	return true
}

// readSST rebuilds the shared string table from the SST record and the CONTINUE
// records that follow it.
//
// A malformed table stops the read and returns whatever was decoded before the
// damage: a workbook whose SST is half-readable still classifies on that half,
// and the alternative -- failing the whole document -- loses text that is right
// there.
func readSST(records []biffRecord) []string {
	start := -1
	for i, r := range records {
		if r.Type == biffSST {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}

	blocks := [][]byte{records[start].Data}
	for i := start + 1; i < len(records) && records[i].Type == biffCONTINUE; i++ {
		blocks = append(blocks, records[i].Data)
	}

	r := &sstReader{blocks: blocks}
	if _, ok := r.u32(); !ok { // cstTotal, not needed
		return nil
	}
	count, ok := r.u32()
	if !ok {
		return nil
	}
	// The count is a 32-bit field from the file. Cap it, and size the slice
	// from the cap rather than from the claim.
	n := int(count)
	if n < 0 || n > maxSSTStrings {
		n = maxSSTStrings
	}

	out := make([]string, 0, min(n, 4096))
	for i := 0; i < n && !r.atEnd(); i++ {
		s, ok := readSSTString(r)
		if !ok {
			break
		}
		out = append(out, s)
	}
	return out
}

// readSSTString reads one XLUnicodeRichExtendedString, handling a split across
// CONTINUE records.
func readSSTString(r *sstReader) (string, bool) {
	cch, ok := r.u16()
	if !ok {
		return "", false
	}
	if int(cch) > maxSSTStringChars {
		return "", false
	}
	flags, ok := r.byteAt()
	if !ok {
		return "", false
	}
	wide := flags&0x01 != 0
	var runs, extra int
	if flags&0x08 != 0 { // fRichSt: a run-formatting array follows the characters
		v, ok := r.u16()
		if !ok {
			return "", false
		}
		runs = int(v)
	}
	if flags&0x04 != 0 { // fExtSt: an Asian phonetic block follows
		v, ok := r.u32()
		if !ok {
			return "", false
		}
		extra = int(v)
		if extra < 0 || extra > MaxOfficeTextBytes {
			return "", false
		}
	}

	var b strings.Builder
	for i := 0; i < int(cch); i++ {
		// A continuation restarts with its own flags byte, and its width can
		// differ from the first half's.
		if r.atBlockStart() && i > 0 {
			f, ok := r.byteAt()
			if !ok {
				return "", false
			}
			wide = f&0x01 != 0
		}
		if wide {
			u, ok := r.u16()
			if !ok {
				return b.String(), false
			}
			b.WriteString(decodeUTF16LE([]byte{byte(u), byte(u >> 8)}))
			continue
		}
		c, ok := r.byteAt()
		if !ok {
			return b.String(), false
		}
		b.WriteString(decodeCP1252([]byte{c}))
	}

	if runs > 0 && !r.skip(runs*4) {
		return b.String(), false
	}
	if extra > 0 && !r.skip(extra) {
		return b.String(), false
	}
	return b.String(), true
}

// extractPPT reads the PowerPoint Document stream's text atoms.
//
// Slide order is not reconstructed. Doing so means resolving the persist
// directory and the slide-listing atoms, which is a second parser's worth of
// work for an ordering a classifier does not use: what matters is that the
// words are present.
func extractPPT(doc *cfbFile, sink *textSink) (int, error) {
	stream, err := doc.stream("PowerPoint Document")
	if err != nil {
		return 0, err
	}
	n := walkPPTRecords(stream, sink, 0)
	return n, nil
}

// walkPPTRecords descends the record tree, emitting each text atom it meets.
//
// Recursion is bounded by maxPPTDepth and every header is bounds-checked before
// the payload is sliced, so a record claiming a four-gigabyte length inside a
// hundred-byte stream stops the walk instead of panicking.
func walkPPTRecords(stream []byte, sink *textSink, depth int) int {
	if depth > maxPPTDepth {
		return 0
	}
	atoms := 0
	for pos := 0; pos+8 <= len(stream) && !sink.full(); {
		verInstance := binary.LittleEndian.Uint16(stream[pos : pos+2])
		typ := binary.LittleEndian.Uint16(stream[pos+2 : pos+4])
		size := binary.LittleEndian.Uint32(stream[pos+4 : pos+8])
		pos += 8
		if uint64(size) > uint64(len(stream)-pos) {
			break
		}
		body := stream[pos : pos+int(size)]
		pos += int(size)

		if verInstance&0x000F == 0x000F {
			atoms += walkPPTRecords(body, sink, depth+1)
			continue
		}
		switch typ {
		case pptTextCharsAtom, pptCStringAtom:
			sink.line(strings.TrimRight(decodeUTF16LE(body), "\x00"))
			atoms++
		case pptTextBytesAtom:
			sink.line(strings.TrimRight(decodeCP1252(body), "\x00"))
			atoms++
		}
	}
	return atoms
}

// decodeUTF16LE decodes little-endian UTF-16, mapping PowerPoint's and Excel's
// in-band control characters (0x0B vertical tab as a line break, 0x0D as a
// paragraph break) to newlines. An odd trailing byte is dropped rather than
// read past.
func decodeUTF16LE(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u := binary.LittleEndian.Uint16(b[i : i+2])
		if u == 0x0B || u == 0x0D {
			u = '\n'
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units))
}

// cp1252High maps the 0x80-0x9F range, the only bytes where Windows-1252
// differs from Latin-1. Everything else in 0xA0-0xFF is its own code point, so
// a 32-entry table is the whole conversion.
//
// This is a table rather than golang.org/x/text/encoding/charmap because
// Constraint 11 forbids a new dependency, and thirty-two runes is not worth
// one.
var cp1252High = [32]rune{
	'€', '�', '‚', 'ƒ', '„', '…', '†', '‡',
	'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�',
	'�', '‘', '’', '“', '”', '•', '–', '—',
	'˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ',
}

// decodeCP1252 converts Windows-1252 bytes to UTF-8, mapping the in-band break
// characters to newlines the same way decodeUTF16LE does.
func decodeCP1252(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		switch {
		case c == 0x0B || c == 0x0D:
			sb.WriteByte('\n')
		case c < 0x80:
			sb.WriteByte(c)
		case c < 0xA0:
			sb.WriteRune(cp1252High[c-0x80])
		default:
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}
