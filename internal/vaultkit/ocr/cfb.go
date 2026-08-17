package ocr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// ErrNotCFB means the bytes are not an OLE2 / Compound File Binary container.
// Legacy .xls and .ppt files are CFB containers; a file that is not one cannot
// be read by this tier, and the caller degrades rather than guessing.
var ErrNotCFB = errors.New("not a compound file (OLE2) container")

// cfbSignature is the eight-byte magic every compound file begins with.
var cfbSignature = [8]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// Sector-allocation sentinels. Anything at or above cfbMaxRegSect is a
// terminator or a reserved marker, never a sector to follow.
const (
	cfbMaxRegSect  = 0xFFFFFFFA
	cfbDIFATSect   = 0xFFFFFFFC
	cfbFATSect     = 0xFFFFFFFD
	cfbEndOfChain  = 0xFFFFFFFE
	cfbFreeSect    = 0xFFFFFFFF
	cfbHeaderSize  = 512
	cfbDirEntrySz  = 128
	cfbMaxSectorSh = 20 // 1 MiB: far beyond the 512/4096 the format ever uses
)

// maxCFBBytes caps how much of a compound file is read into memory.
//
// Every parser here works on a fully buffered file because chain following
// jumps backwards as often as forwards, and a bounded buffer is the cheapest
// way to make every offset check a slice-length check. 64 MiB is comfortably
// larger than any real legacy .xls or .ppt and small enough that a hostile file
// cannot exhaust memory.
const maxCFBBytes = 64 << 20

// maxCFBDirEntries bounds the directory, which a malformed file can otherwise
// claim is effectively unbounded.
const maxCFBDirEntries = 1 << 16

// cfbEntry is one directory entry: a storage (directory), a stream (file) or
// the root.
type cfbEntry struct {
	Name  string
	Type  byte // 0 empty, 1 storage, 2 stream, 5 root
	Start uint32
	Size  uint64
}

// cfbFile is a parsed compound file: its sector tables, its directory and the
// mini stream, all validated against the actual file length.
//
// Nothing in here trusts a length or an index read from the file. Every sector
// number is range-checked before use and every chain walk is bounded by the
// number of sectors that exist, so a cyclic FAT terminates with an error
// instead of spinning.
type cfbFile struct {
	data       []byte
	sectorSize int
	miniCutoff uint32
	fat        []uint32
	miniFAT    []uint32
	entries    []cfbEntry
	miniStream []byte
}

// openCFB parses the header, sector tables and directory of a compound file.
// It returns ErrNotCFB for anything that is not one, and a descriptive error
// -- never a panic -- for a file that is one but is damaged or hostile.
func openCFB(data []byte) (*cfbFile, error) {
	if len(data) < cfbHeaderSize {
		return nil, fmt.Errorf("%w (the file is %d bytes, shorter than the 512-byte header)", ErrNotCFB, len(data))
	}
	if string(data[:8]) != string(cfbSignature[:]) {
		return nil, ErrNotCFB
	}

	shift := binary.LittleEndian.Uint16(data[30:32])
	if shift < 7 || shift > cfbMaxSectorSh {
		return nil, fmt.Errorf("compound file: sector shift %d is out of range", shift)
	}
	miniShift := binary.LittleEndian.Uint16(data[32:34])
	if miniShift < 2 || miniShift >= shift {
		return nil, fmt.Errorf("compound file: mini sector shift %d is out of range", miniShift)
	}

	f := &cfbFile{
		data:       data,
		sectorSize: 1 << shift,
		miniCutoff: binary.LittleEndian.Uint32(data[56:60]),
	}
	if f.miniCutoff == 0 || f.miniCutoff > 1<<20 {
		f.miniCutoff = 4096
	}
	if f.sectorCount() == 0 {
		return nil, fmt.Errorf("%w (the file is truncated: no sectors follow the header)", ErrNotCFB)
	}

	if err := f.readFAT(); err != nil {
		return nil, err
	}
	if err := f.readMiniFAT(); err != nil {
		return nil, err
	}
	if err := f.readDirectory(); err != nil {
		return nil, err
	}
	f.readMiniStream()
	return f, nil
}

// sectorCount is how many whole sectors the file actually contains. A trailing
// partial sector is not counted: reading it would read past the buffer.
func (f *cfbFile) sectorCount() int {
	if len(f.data) <= cfbHeaderSize {
		return 0
	}
	return (len(f.data) - cfbHeaderSize) / f.sectorSize
}

// sector returns sector n's bytes, or false when n is not a sector this file
// actually has. This is the single place a sector number becomes an offset,
// so it is the single place that check has to be right.
func (f *cfbFile) sector(n uint32) ([]byte, bool) {
	if n > cfbMaxRegSect || int(n) >= f.sectorCount() {
		return nil, false
	}
	off := cfbHeaderSize + int(n)*f.sectorSize
	return f.data[off : off+f.sectorSize], true
}

// readFAT assembles the file allocation table from the DIFAT: 109 entries in
// the header, then any DIFAT sectors chained after it.
func (f *cfbFile) readFAT() error {
	numFAT := binary.LittleEndian.Uint32(data32(f.data, 44))
	fatSectors := make([]uint32, 0, 128)

	for i := 0; i < 109; i++ {
		s := binary.LittleEndian.Uint32(f.data[76+i*4 : 80+i*4])
		if s > cfbMaxRegSect {
			continue
		}
		fatSectors = append(fatSectors, s)
	}

	// Follow the DIFAT chain. Bounded by the sector count and guarded by a
	// visited set: a DIFAT sector that points at itself is a real corruption
	// mode and must stop rather than loop.
	next := binary.LittleEndian.Uint32(f.data[68:72])
	seen := make(map[uint32]bool)
	perSector := f.sectorSize/4 - 1
	for hops := 0; next <= cfbMaxRegSect && hops <= f.sectorCount(); hops++ {
		if seen[next] {
			return errors.New("compound file: the DIFAT chain is cyclic")
		}
		seen[next] = true
		sec, ok := f.sector(next)
		if !ok {
			return fmt.Errorf("compound file: DIFAT sector %d is past the end of the file", next)
		}
		for i := 0; i < perSector; i++ {
			s := binary.LittleEndian.Uint32(sec[i*4 : i*4+4])
			if s > cfbMaxRegSect {
				continue
			}
			fatSectors = append(fatSectors, s)
		}
		next = binary.LittleEndian.Uint32(sec[perSector*4 : perSector*4+4])
	}

	// numFAT is a hint, not an authority: honour it only when it trims.
	if numFAT > 0 && int(numFAT) < len(fatSectors) {
		fatSectors = fatSectors[:numFAT]
	}

	f.fat = make([]uint32, 0, len(fatSectors)*(f.sectorSize/4))
	for _, s := range fatSectors {
		sec, ok := f.sector(s)
		if !ok {
			// A FAT sector past the end means the rest of the table is
			// unreadable; keep what is valid rather than failing the file.
			break
		}
		for i := 0; i+4 <= len(sec); i += 4 {
			f.fat = append(f.fat, binary.LittleEndian.Uint32(sec[i:i+4]))
		}
	}
	if len(f.fat) == 0 {
		return errors.New("compound file: the file allocation table is empty or unreadable")
	}
	return nil
}

// readMiniFAT reads the mini-FAT chain, which allocates the mini stream that
// holds every stream smaller than the cutoff.
func (f *cfbFile) readMiniFAT() error {
	start := binary.LittleEndian.Uint32(f.data[60:64])
	raw, err := f.readChain(start, 0)
	if err != nil {
		// A broken mini-FAT only costs us the small streams; the Workbook and
		// PowerPoint Document streams are large and live in the main FAT.
		return nil
	}
	f.miniFAT = make([]uint32, 0, len(raw)/4)
	for i := 0; i+4 <= len(raw); i += 4 {
		f.miniFAT = append(f.miniFAT, binary.LittleEndian.Uint32(raw[i:i+4]))
	}
	return nil
}

// readChain walks a FAT chain from start and returns its bytes, stopping at
// limit bytes when limit is non-zero.
//
// Two independent bounds make a hostile chain terminate: a visited set catches
// a cycle exactly, and a hop budget catches a chain that is merely absurdly
// long. Either one alone would be enough for the common case; both together
// mean neither a self-referential sector nor a million-hop chain can hang an
// ingest run.
func (f *cfbFile) readChain(start uint32, limit int) ([]byte, error) {
	if start > cfbMaxRegSect {
		return nil, nil
	}
	var out []byte
	visited := make(map[uint32]bool)
	budget := f.sectorCount() + 1
	for cur := start; cur <= cfbMaxRegSect; {
		if visited[cur] {
			return out, fmt.Errorf("compound file: the FAT chain is cyclic at sector %d", cur)
		}
		visited[cur] = true
		if len(visited) > budget {
			return out, errors.New("compound file: the FAT chain is longer than the file has sectors")
		}
		sec, ok := f.sector(cur)
		if !ok {
			return out, fmt.Errorf("compound file: sector %d is past the end of the file", cur)
		}
		out = append(out, sec...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
		if int(cur) >= len(f.fat) {
			return out, fmt.Errorf("compound file: sector %d has no allocation-table entry", cur)
		}
		cur = f.fat[cur]
	}
	return out, nil
}

// readMiniChain is readChain for the mini stream, whose sectors are 64 bytes
// and indexed by the mini FAT.
func (f *cfbFile) readMiniChain(start uint32, size int) ([]byte, error) {
	const miniSize = 64
	if start > cfbMaxRegSect || len(f.miniStream) == 0 {
		return nil, nil
	}
	var out []byte
	visited := make(map[uint32]bool)
	for cur := start; cur <= cfbMaxRegSect; {
		if visited[cur] {
			return out, fmt.Errorf("compound file: the mini-FAT chain is cyclic at sector %d", cur)
		}
		visited[cur] = true
		off := int(cur) * miniSize
		if off < 0 || off+miniSize > len(f.miniStream) {
			return out, fmt.Errorf("compound file: mini sector %d is past the end of the mini stream", cur)
		}
		out = append(out, f.miniStream[off:off+miniSize]...)
		if size > 0 && len(out) >= size {
			return out[:size], nil
		}
		if int(cur) >= len(f.miniFAT) {
			return out, fmt.Errorf("compound file: mini sector %d has no mini-FAT entry", cur)
		}
		cur = f.miniFAT[cur]
	}
	return out, nil
}

// readDirectory parses the directory chain into entries.
func (f *cfbFile) readDirectory() error {
	start := binary.LittleEndian.Uint32(f.data[48:52])
	raw, err := f.readChain(start, maxCFBDirEntries*cfbDirEntrySz)
	if err != nil && len(raw) == 0 {
		return err
	}
	for off := 0; off+cfbDirEntrySz <= len(raw) && len(f.entries) < maxCFBDirEntries; off += cfbDirEntrySz {
		e := raw[off : off+cfbDirEntrySz]
		typ := e[66]
		if typ == 0 {
			continue
		}
		nameLen := int(binary.LittleEndian.Uint16(e[64:66]))
		if nameLen < 2 || nameLen > 64 {
			continue
		}
		units := make([]uint16, 0, nameLen/2-1)
		for i := 0; i+1 < nameLen-2; i += 2 {
			units = append(units, binary.LittleEndian.Uint16(e[i:i+2]))
		}
		f.entries = append(f.entries, cfbEntry{
			Name:  strings.TrimRight(string(utf16.Decode(units)), "\x00"),
			Type:  typ,
			Start: binary.LittleEndian.Uint32(e[116:120]),
			Size:  binary.LittleEndian.Uint64(e[120:128]),
		})
	}
	if len(f.entries) == 0 {
		return errors.New("compound file: the directory is empty or unreadable")
	}
	return nil
}

// readMiniStream loads the root entry's stream, which is the backing store for
// every small stream in the file.
func (f *cfbFile) readMiniStream() {
	for _, e := range f.entries {
		if e.Type != 5 {
			continue
		}
		size := clampSize(e.Size, maxCFBBytes)
		raw, _ := f.readChain(e.Start, size)
		if size > 0 && len(raw) > size {
			raw = raw[:size]
		}
		f.miniStream = raw
		return
	}
}

// stream returns the named stream's bytes. Lookup is case-insensitive and
// ignores the storage hierarchy: the streams this package wants ("Workbook",
// "PowerPoint Document") are top-level in every real file, and matching by name
// avoids reconstructing the red-black directory tree for no benefit.
func (f *cfbFile) stream(name string) ([]byte, error) {
	for _, e := range f.entries {
		if e.Type != 2 || !strings.EqualFold(e.Name, name) {
			continue
		}
		size := clampSize(e.Size, maxCFBBytes)
		if size == 0 {
			return nil, fmt.Errorf("compound file: the %q stream is empty", name)
		}
		var raw []byte
		var err error
		if e.Size < uint64(f.miniCutoff) {
			raw, err = f.readMiniChain(e.Start, size)
		} else {
			raw, err = f.readChain(e.Start, size)
		}
		// A chain that was cyclic, ran off the end of the file or lost its
		// allocation entry fails the stream rather than yielding the prefix it
		// managed. The prefix is not a shorter document: it is the readable part
		// of a file whose structure is provably wrong, and passing it on would
		// classify a document on evidence the container itself contradicts.
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("compound file: the %q stream could not be read", name)
		}
		if len(raw) > size {
			raw = raw[:size]
		}
		return raw, nil
	}
	return nil, fmt.Errorf("compound file: no %q stream", name)
}

// hasStream reports whether a stream with this name exists, without reading it.
func (f *cfbFile) hasStream(name string) bool {
	for _, e := range f.entries {
		if e.Type == 2 && strings.EqualFold(e.Name, name) {
			return true
		}
	}
	return false
}

// clampSize turns an untrusted 64-bit size field into an allocation-safe int.
// A stream header can claim gigabytes; nothing may be sized from it directly.
func clampSize(size uint64, max int) int {
	if size > uint64(max) {
		return max
	}
	return int(size)
}

// data32 returns a four-byte window at off, or four zero bytes when the buffer
// is too short, so a header read can never panic on a truncated file.
func data32(b []byte, off int) []byte {
	if off < 0 || off+4 > len(b) {
		return []byte{0, 0, 0, 0}
	}
	return b[off : off+4]
}
