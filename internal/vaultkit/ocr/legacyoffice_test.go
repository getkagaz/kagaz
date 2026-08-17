package ocr

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf16"
)

// --- fixture construction -------------------------------------------------
//
// There is no way to write a genuine BIFF8 workbook from Go or from Python's
// standard library, so the .xls fixtures here are built by hand: a real OLE2
// container carrying real BIFF records, but assembled by this file rather than
// by Excel. That is a real limit on what these tests prove -- they prove the
// parser reads the format as specified, not that it reads whatever Excel 97
// actually emitted. The round trip against a tool-produced file is the
// textutil `.doc` test, which is why that one exists.

// cfbOptions are the deliberate corruptions a hostile-input test needs.
type cfbOptions struct {
	// cyclicFAT makes the stream's first sector point at itself.
	cyclicFAT bool
	// streamSize overrides the declared stream size, so a directory entry can
	// claim gigabytes the file does not contain.
	streamSize uint64
}

// buildCFB wraps payload in a minimal but valid compound file whose single
// stream is called name.
//
// The layout is the simplest one that exercises the real code path: 512-byte
// sectors, the payload in consecutive normal (not mini) sectors, one FAT sector
// and one directory sector after it.
func buildCFB(name string, payload []byte, opt cfbOptions) []byte {
	const sectorSize = 512
	// The mini-stream cutoff is 4096, so the payload must clear it to be
	// allocated from the main FAT.
	if len(payload) < 4096 {
		payload = append(payload, make([]byte, 4096-len(payload))...)
	}
	dataSectors := (len(payload) + sectorSize - 1) / sectorSize
	payload = append(payload, make([]byte, dataSectors*sectorSize-len(payload))...)

	fatSector := uint32(dataSectors)
	dirSector := fatSector + 1
	totalSectors := int(dirSector) + 1

	out := make([]byte, sectorSize+totalSectors*sectorSize)
	hdr := out[:sectorSize]
	copy(hdr, cfbSignature[:])
	binary.LittleEndian.PutUint16(hdr[26:28], 0x003E) // minor version
	binary.LittleEndian.PutUint16(hdr[28:30], 0xFFFE) // little endian
	binary.LittleEndian.PutUint16(hdr[30:32], 9)      // 512-byte sectors
	binary.LittleEndian.PutUint16(hdr[32:34], 6)      // 64-byte mini sectors
	binary.LittleEndian.PutUint32(hdr[44:48], 1)      // one FAT sector
	binary.LittleEndian.PutUint32(hdr[48:52], dirSector)
	binary.LittleEndian.PutUint32(hdr[56:60], 4096) // mini-stream cutoff
	binary.LittleEndian.PutUint32(hdr[60:64], cfbEndOfChain)
	binary.LittleEndian.PutUint32(hdr[64:68], 0)
	binary.LittleEndian.PutUint32(hdr[68:72], cfbEndOfChain)
	binary.LittleEndian.PutUint32(hdr[72:76], 0)
	binary.LittleEndian.PutUint32(hdr[76:80], fatSector) // DIFAT[0]
	for i := 1; i < 109; i++ {
		binary.LittleEndian.PutUint32(hdr[76+i*4:80+i*4], cfbFreeSect)
	}

	copy(out[sectorSize:], payload)

	fat := out[sectorSize+int(fatSector)*sectorSize:][:sectorSize]
	for i := 0; i < sectorSize/4; i++ {
		binary.LittleEndian.PutUint32(fat[i*4:], cfbFreeSect)
	}
	for i := 0; i < dataSectors; i++ {
		next := uint32(i + 1)
		if i == dataSectors-1 {
			next = cfbEndOfChain
		}
		binary.LittleEndian.PutUint32(fat[i*4:], next)
	}
	if opt.cyclicFAT {
		binary.LittleEndian.PutUint32(fat[0:], 0) // sector 0 points at itself
	}
	binary.LittleEndian.PutUint32(fat[int(fatSector)*4:], cfbFATSect)
	binary.LittleEndian.PutUint32(fat[int(dirSector)*4:], cfbEndOfChain)

	dir := out[sectorSize+int(dirSector)*sectorSize:][:sectorSize]
	writeDirEntry(dir[0:128], "Root Entry", 5, cfbEndOfChain, 0)
	size := uint64(len(payload))
	if opt.streamSize != 0 {
		size = opt.streamSize
	}
	writeDirEntry(dir[128:256], name, 2, 0, size)
	for i := 2; i < 4; i++ {
		dir[i*128+66] = 0 // unallocated
	}
	return out
}

// writeDirEntry fills one 128-byte directory entry.
func writeDirEntry(e []byte, name string, typ byte, start uint32, size uint64) {
	units := utf16.Encode([]rune(name))
	for i, u := range units {
		binary.LittleEndian.PutUint16(e[i*2:], u)
	}
	binary.LittleEndian.PutUint16(e[64:66], uint16(len(units)*2+2))
	e[66] = typ
	e[67] = 1 // black
	binary.LittleEndian.PutUint32(e[68:72], cfbFreeSect)
	binary.LittleEndian.PutUint32(e[72:76], cfbFreeSect)
	binary.LittleEndian.PutUint32(e[76:80], cfbFreeSect)
	binary.LittleEndian.PutUint32(e[116:120], start)
	binary.LittleEndian.PutUint64(e[120:128], size)
}

// biffRec renders one BIFF record.
func biffRec(typ uint16, body []byte) []byte {
	out := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint16(out[0:2], typ)
	binary.LittleEndian.PutUint16(out[2:4], uint16(len(body)))
	return append(out, body...)
}

// u16 and u32 are little-endian encoders for fixture assembly.
func u16(v uint16) []byte { b := make([]byte, 2); binary.LittleEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }

// sstBody builds an SST payload holding the given single-byte strings.
func sstBody(strs []string) []byte {
	body := append(u32(uint32(len(strs))), u32(uint32(len(strs)))...)
	for _, s := range strs {
		body = append(body, u16(uint16(len(s)))...)
		body = append(body, 0x00) // fHighByte clear: one byte per character
		body = append(body, []byte(s)...)
	}
	return body
}

// buildWorkbook assembles a BIFF8 workbook stream: a sheet name, a shared
// string table and one row of labels and numbers.
func buildWorkbook() []byte {
	var s []byte
	s = append(s, biffRec(0x0809, append(u16(0x0600), make([]byte, 14)...))...) // BOF

	sheet := []byte("Invoices")
	bs := append(u32(0), u16(0)...)
	bs = append(bs, byte(len(sheet)), 0x00)
	bs = append(bs, sheet...)
	s = append(s, biffRec(biffBOUNDSHET, bs)...)

	s = append(s, biffRec(biffSST, sstBody([]string{"Invoice Number", "Acme Industries Pvt Ltd"}))...)

	// Row 0: two shared-string labels.
	s = append(s, biffRec(biffLABELSST, concat(u16(0), u16(0), u16(15), u32(0)))...)
	s = append(s, biffRec(biffLABELSST, concat(u16(0), u16(1), u16(15), u32(1)))...)

	// Row 1: an inline label, an RK integer, an RK float and a NUMBER.
	label := []byte("Total Amount Due")
	s = append(s, biffRec(biffLABEL, concat(u16(1), u16(0), u16(15),
		u16(uint16(len(label))), []byte{0x00}, label))...)
	s = append(s, biffRec(biffRK, concat(u16(1), u16(1), u16(15), u32(42<<2|0x02)))...)
	s = append(s, biffRec(biffRK, concat(u16(1), u16(2), u16(15),
		u32(uint32(math.Float64bits(12.5)>>32)&0xFFFFFFFC)))...)
	num := make([]byte, 8)
	binary.LittleEndian.PutUint64(num, math.Float64bits(45300))
	s = append(s, biffRec(biffNUMBER, concat(u16(1), u16(3), u16(15), num))...)

	s = append(s, biffRec(biffEOF, nil)...)
	return s
}

// rkInt encodes v as an RK integer. It is a function rather than a constant
// expression because a negative value's two's-complement bit pattern is not a
// uint32 constant.
func rkInt(v int32) uint32 { return uint32(v)<<2 | 0x02 }

// concat joins byte slices.
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// --- tests ----------------------------------------------------------------

// TestLegacyOfficeXLS: a BIFF8 workbook yields its sheet name, its shared
// strings, its inline label and every flavour of number.
func TestLegacyOfficeXLS(t *testing.T) {
	path := writeTemp(t, "book.xls", buildCFB("Workbook", buildWorkbook(), cfbOptions{}))

	res, err := (&LegacyOffice{}).Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Engine != "legacyoffice" {
		t.Errorf("Engine = %q, want legacyoffice", res.Engine)
	}
	t.Logf("extracted:\n%s", res.Text)
	for _, want := range []string{
		"Sheet: Invoices",
		"Invoice Number",
		"Acme Industries Pvt Ltd",
		"Total Amount Due",
		"42",
		"12.5",
		"45300",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text is missing %q:\n%s", want, res.Text)
		}
	}
	// Rows must stay rows: a label and its value on one line is the pairing a
	// classifier reads.
	if !strings.Contains(res.Text, "Total Amount Due\t42\t12.5\t45300") {
		t.Errorf("row 1 did not come out as one row:\n%s", res.Text)
	}
}

// TestLegacyOfficeXLSWideAndSplitStrings: an SST string split across a CONTINUE
// record, whose continuation declares a different character width, is decoded
// whole. This is the failure mode a reader that merely concatenated payloads
// would turn into garbage.
func TestLegacyOfficeXLSWideAndSplitStrings(t *testing.T) {
	const head = "Consolidated Bal"
	const tail = "ance Sheet 2003"

	sst := append(u32(1), u32(1)...)
	sst = append(sst, u16(uint16(len(head)+len(tail)))...)
	sst = append(sst, 0x00) // first half: one byte per character
	sst = append(sst, []byte(head)...)

	// The continuation restarts with its own flags byte, this time wide.
	cont := []byte{0x01}
	for _, r := range tail {
		cont = append(cont, u16(uint16(r))...)
	}

	var s []byte
	s = append(s, biffRec(0x0809, append(u16(0x0600), make([]byte, 14)...))...)
	s = append(s, biffRec(biffSST, sst)...)
	s = append(s, biffRec(biffCONTINUE, cont)...)
	s = append(s, biffRec(biffLABELSST, concat(u16(0), u16(0), u16(15), u32(0)))...)
	s = append(s, biffRec(biffEOF, nil)...)

	path := writeTemp(t, "split.xls", buildCFB("Workbook", s, cfbOptions{}))
	res, err := (&LegacyOffice{}).Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(res.Text, head+tail) {
		t.Errorf("the split string did not survive: %q", res.Text)
	}
}

// TestLegacyOfficePPT: TextCharsAtom and TextBytesAtom records inside a
// container are both found and decoded.
func TestLegacyOfficePPT(t *testing.T) {
	pptRec := func(verInstance, typ uint16, body []byte) []byte {
		out := concat(u16(verInstance), u16(typ), u32(uint32(len(body))))
		return append(out, body...)
	}

	var wide []byte
	for _, r := range "Quarterly Review" {
		wide = append(wide, u16(uint16(r))...)
	}
	inner := concat(
		pptRec(0x0000, pptTextCharsAtom, wide),
		// CP1252 0x92 is a right single quote, not Latin-1's private-use byte.
		pptRec(0x0000, pptTextBytesAtom, []byte("Acme\x92s revenue")),
	)
	stream := pptRec(0x000F, 0x03E8, inner) // a container wrapping both atoms

	path := writeTemp(t, "deck.ppt", buildCFB("PowerPoint Document", stream, cfbOptions{}))
	res, err := (&LegacyOffice{}).Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(res.Text, "Quarterly Review") {
		t.Errorf("TextCharsAtom missing: %q", res.Text)
	}
	if !strings.Contains(res.Text, "Acme’s revenue") {
		t.Errorf("TextBytesAtom missing or mis-decoded: %q", res.Text)
	}
}

// TestLegacyOfficeHostileInputs: every malformed, truncated and adversarial
// shape must produce an error rather than a panic, a hang or an allocation the
// file talked us into.
func TestLegacyOfficeHostileInputs(t *testing.T) {
	good := buildCFB("Workbook", buildWorkbook(), cfbOptions{})

	// A directory entry claiming a 4 GiB stream. Nothing may be sized from it.
	absurd := buildCFB("Workbook", buildWorkbook(), cfbOptions{streamSize: 1 << 32})

	// A BIFF record whose declared length runs past the end of the stream.
	overrun := buildCFB("Workbook", concat(
		biffRec(0x0809, append(u16(0x0600), make([]byte, 14)...)),
		u16(biffLABEL), u16(0xFFFF), []byte("short"),
	), cfbOptions{})

	// An SST claiming four billion strings in a handful of bytes.
	liarSST := buildCFB("Workbook", concat(
		biffRec(0x0809, append(u16(0x0600), make([]byte, 14)...)),
		biffRec(biffSST, concat(u32(0xFFFFFFFF), u32(0xFFFFFFFF), u16(0x7FFF), []byte{0x01})),
	), cfbOptions{})

	tests := []struct {
		name    string
		ext     string
		data    []byte
		wantErr bool
	}{
		{"empty", ".xls", nil, true},
		{"one byte", ".xls", []byte{0xD0}, true},
		{"signature only", ".xls", cfbSignature[:], true},
		{"header only", ".xls", good[:512], true},
		{"truncated mid-stream", ".xls", good[:len(good)/2], true},
		{"not a compound file", ".xls", []byte(strings.Repeat("PK\x03\x04", 512)), true},
		{"header plus zeros", ".xls", append(append([]byte{}, cfbSignature[:]...), make([]byte, 8192)...), true},
		{"cyclic FAT", ".xls", buildCFB("Workbook", buildWorkbook(), cfbOptions{cyclicFAT: true}), true},
		{"absurd stream size", ".xls", absurd, false},
		{"record length overrun", ".xls", overrun, true},
		{"SST claims 4 billion strings", ".xls", liarSST, true},
		{"ppt of nonsense", ".ppt", append(append([]byte{}, good[:512]...), make([]byte, 4096)...), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, "hostile"+tc.ext, tc.data)
			res, err := (&LegacyOffice{}).Extract(context.Background(), path)
			if tc.wantErr && err == nil {
				t.Fatalf("no error; extracted %d bytes of text", len(res.Text))
			}
			if err != nil {
				// Logged so a reader can see the refusals are legible, which is
				// half the requirement: not panicking is the other half.
				t.Logf("refused: %v", err)
			}
		})
	}
}

// TestLegacyOfficeCyclicFATNamesItself: the cyclic-chain guard must say what it
// found, because "no text" would send the user looking at the wrong thing.
func TestLegacyOfficeCyclicFATNamesItself(t *testing.T) {
	path := writeTemp(t, "cycle.xls", buildCFB("Workbook", buildWorkbook(), cfbOptions{cyclicFAT: true}))
	_, err := (&LegacyOffice{}).Extract(context.Background(), path)
	if err == nil {
		t.Fatal("a cyclic FAT chain was accepted")
	}
	if !strings.Contains(err.Error(), "cyclic") {
		t.Errorf("the error does not name the cycle: %v", err)
	}
}

// TestOpenCFBRandomBytes: random and truncated inputs, deterministically
// seeded, must all return an error rather than crash. This is the cheap
// stand-in for a fuzzer on machines that do not run one.
func TestOpenCFBRandomBytes(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	good := buildCFB("Workbook", buildWorkbook(), cfbOptions{})

	for i := 0; i < 512; i++ {
		buf := make([]byte, r.Intn(2048))
		_, _ = r.Read(buf)
		if _, err := openCFB(buf); err == nil {
			t.Fatalf("random buffer %d of %d bytes parsed as a compound file", i, len(buf))
		}

		// A real file with one byte flipped: the shape that gets past the
		// signature check and into the parser proper.
		mutated := append([]byte{}, good...)
		mutated[r.Intn(len(mutated))] = byte(r.Intn(256))
		if doc, err := openCFB(mutated); err == nil {
			sink := &textSink{limit: MaxOfficeTextBytes}
			_, _ = extractXLS(doc, sink)
			_, _ = extractPPT(doc, sink)
		}

		// The same file truncated at an arbitrary point.
		cut := good[:r.Intn(len(good))]
		if doc, err := openCFB(cut); err == nil {
			sink := &textSink{limit: MaxOfficeTextBytes}
			_, _ = extractXLS(doc, sink)
		}
	}
}

// FuzzLegacyOffice explores the compound-file and BIFF parsers directly. It is
// seeded with a valid workbook so the fuzzer starts past the signature check
// rather than spending its budget rediscovering eight magic bytes.
func FuzzLegacyOffice(f *testing.F) {
	f.Add(buildCFB("Workbook", buildWorkbook(), cfbOptions{}))
	f.Add(buildCFB("PowerPoint Document", buildWorkbook(), cfbOptions{}))
	f.Add(cfbSignature[:])
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := openCFB(data)
		if err != nil {
			return
		}
		sink := &textSink{limit: MaxOfficeTextBytes}
		_, _ = extractXLS(doc, sink)
		_, _ = extractPPT(doc, sink)
		if sink.b.Len() > MaxOfficeTextBytes {
			t.Fatalf("the sink overran its cap: %d bytes", sink.b.Len())
		}
	})
}

// TestLegacyOfficeNotCompound: a file that is not a compound file at all must
// say so by name, not as a generic failure.
func TestLegacyOfficeNotCompound(t *testing.T) {
	path := writeTemp(t, "fake.xls", []byte("this is a CSV, honestly\n1,2,3\n"))
	_, err := (&LegacyOffice{}).Extract(context.Background(), path)
	if !errors.Is(err, ErrNotCFB) {
		t.Fatalf("error = %v, want it to wrap ErrNotCFB", err)
	}
	if !strings.Contains(err.Error(), "Excel 97-2003") {
		t.Errorf("the error does not name the format: %v", err)
	}
}

// TestDecodeRK covers the four RK encodings, which are easy to get subtly wrong
// and would silently misreport every number in a workbook.
func TestDecodeRK(t *testing.T) {
	tests := []struct {
		name string
		rk   uint32
		want float64
	}{
		{"integer", rkInt(42), 42},
		{"integer divided by 100", rkInt(4530) | 0x01, 45.30},
		{"negative integer", rkInt(-7), -7},
		{"float", uint32(math.Float64bits(12.5)>>32) & 0xFFFFFFFC, 12.5},
		{"float divided by 100", (uint32(math.Float64bits(1250)>>32) & 0xFFFFFFFC) | 0x01, 12.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeRK(tc.rk); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("decodeRK(%#x) = %v, want %v", tc.rk, got, tc.want)
			}
		})
	}
}

// TestDecodeCP1252 checks the one range where Windows-1252 is not Latin-1, plus
// the in-band break characters legacy Office uses instead of newlines.
func TestDecodeCP1252(t *testing.T) {
	tests := []struct {
		in   []byte
		want string
	}{
		{[]byte{0x80}, "€"},
		{[]byte{0x92}, "’"},
		{[]byte{0x97}, "—"},
		{[]byte{0xA9}, "©"},
		{[]byte{0xFF}, "ÿ"},
		{[]byte{'a', 0x0D, 'b', 0x0B, 'c'}, "a\nb\nc"},
	}
	for _, tc := range tests {
		if got := decodeCP1252(tc.in); got != tc.want {
			t.Errorf("decodeCP1252(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
