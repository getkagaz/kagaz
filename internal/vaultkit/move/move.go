// Package move is the only code in Kagaz that relocates a file. It implements
// safety invariants 2-4: nothing is ever unlinked (sources are renamed into a
// staging folder the user empties from Finder), every operation writes its
// manifest before touching a byte, and every copy is SHA256-verified.
//
// Moves are byte copies rather than renames on purpose: a rename can lose the
// digital signature on some encrypted PDFs, and behaves inconsistently across
// FUSE mounts and iCloud Drive.
package move

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// Collision decides what happens when the destination already exists.
type Collision int

// Collision policies.
const (
	// CollisionFail aborts the whole operation. The default: silently choosing
	// for the user is how documents get lost.
	CollisionFail Collision = iota
	// CollisionSuffix appends _2, _3, … to the destination stem.
	CollisionSuffix
	// CollisionSkip leaves the source where it is.
	CollisionSkip
)

// Op is one planned relocation.
type Op struct {
	Src string
	Dst string
}

// Row is one manifest line: where the file is now, where it came from, and the
// SHA256 that was verified across the copy.
type Row struct {
	CurrentPath  string
	OriginalPath string
	SHA256       string
}

// Manifest records an executed (or partially executed) operation.
type Manifest struct {
	Path string
	Op   string
	Rows []Row
}

// Result reports what Execute actually did.
type Result struct {
	Manifest *Manifest
	Moved    []Op
	Skipped  []Op
	Warnings []string
}

// Engine performs moves for one vault.
type Engine struct {
	// ManifestDir is where manifests are written.
	ManifestDir string
	// StagingDir receives every superseded source file. Never emptied by Kagaz.
	StagingDir string
	// Now supplies timestamps; tests substitute a fixed clock.
	Now func() time.Time
	// DryRun plans and returns a manifest without writing anything.
	DryRun bool
	// OnCollision selects the destination-exists policy.
	OnCollision Collision
}

// New builds an Engine with the standard vault paths.
func New(manifestDir, stagingDir string) *Engine {
	return &Engine{
		ManifestDir: manifestDir,
		StagingDir:  stagingDir,
		Now:         time.Now,
	}
}

// ErrDestinationExists is returned under CollisionFail.
var ErrDestinationExists = errors.New("destination already exists")

// Execute performs ops atomically per-file: copy, verify, carry tags across,
// then stage the source. The manifest is written before the first byte moves,
// so a crash leaves a resumable record on disk.
func (e *Engine) Execute(opName string, ops []Op) (*Result, error) {
	if len(ops) == 0 {
		return &Result{Manifest: &Manifest{Op: opName}}, nil
	}

	planned := make([]Op, 0, len(ops))
	rows := make([]Row, 0, len(ops))
	res := &Result{}

	// claimed records the destinations earlier ops in THIS batch have already
	// taken, mapped to the SHA256 of the source that will land there. Collisions
	// must be resolved against the union of "exists on disk" and "claimed by
	// this batch": nothing has moved yet while planning, so the filesystem alone
	// cannot see a second op aiming at the same destination, and without this the
	// second copy would silently overwrite the first document and leave the first
	// manifest row recording a hash the file no longer has.
	claimed := make(map[string]string, len(ops))

	for _, op := range ops {
		src, err := filepath.Abs(op.Src)
		if err != nil {
			return nil, err
		}
		dst, err := filepath.Abs(op.Dst)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", op.Src, err)
		}
		if st.IsDir() {
			return nil, fmt.Errorf("source %s is a directory; kagaz moves files", op.Src)
		}
		if src == dst {
			res.Skipped = append(res.Skipped, Op{Src: src, Dst: dst})
			continue
		}

		dst, skip, err := e.resolveCollision(src, dst, claimed)
		if err != nil {
			return nil, err
		}
		if skip {
			res.Skipped = append(res.Skipped, Op{Src: src, Dst: dst})
			continue
		}

		sum, err := SHA256(src)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", src, err)
		}
		claimed[dst] = sum
		planned = append(planned, Op{Src: src, Dst: dst})
		rows = append(rows, Row{CurrentPath: dst, OriginalPath: src, SHA256: sum})
	}

	man := &Manifest{Op: opName, Rows: rows}
	if len(planned) == 0 {
		res.Manifest = man
		return res, nil
	}

	if e.DryRun {
		res.Manifest = man
		res.Moved = planned
		return res, nil
	}

	path, err := e.writeManifest(opName, rows)
	if err != nil {
		return nil, err
	}
	man.Path = path

	for i, op := range planned {
		if err := e.moveOne(op.Src, op.Dst, rows[i].SHA256, res); err != nil {
			res.Manifest = man
			return res, fmt.Errorf("%s -> %s: %w (manifest: %s)", op.Src, op.Dst, err, path)
		}
		res.Moved = append(res.Moved, op)
	}
	res.Manifest = man
	return res, nil
}

// afterCopyHook, when non-nil, runs immediately after a destination has been
// written and before its SHA256 is verified. It is a test seam and nothing
// else: it is never assigned outside of tests, and in production it costs one
// nil comparison per file.
//
// It exists because the checksum-mismatch branch below is the single most
// safety-critical path in Kagaz — it is what guarantees invariant 5, "a
// mismatch aborts and leaves the source untouched" — and there is no portable
// way to make a real filesystem corrupt a freshly written file on demand.
// Without this seam that branch could only ever be reviewed, never executed.
// The seam does not weaken the production path: the copy, the hash and the
// comparison are unchanged, and a nil hook makes the code behave exactly as if
// it were absent.
var afterCopyHook func(dst string)

// moveOne copies, verifies, transfers tags, then stages the source.
func (e *Engine) moveOne(src, dst, wantSum string, res *Result) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := CopyFile(src, dst); err != nil {
		return err
	}
	if afterCopyHook != nil {
		afterCopyHook(dst)
	}
	gotSum, err := SHA256(dst)
	if err != nil {
		return err
	}
	if gotSum != wantSum {
		// The copy is corrupt: remove it and leave the source untouched.
		_ = os.Remove(dst)
		return fmt.Errorf("checksum mismatch after copy (want %s, got %s); destination removed, source untouched", wantSum[:12], gotSum[:12])
	}
	if err := tags.Copy(src, dst); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: could not carry Finder tags across: %v", filepath.Base(dst), err))
	}
	// Sidecars travel with their document.
	if err := e.moveSidecar(src, dst); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: sidecar not moved: %v", filepath.Base(src), err))
	}
	return e.stage(src)
}

// moveSidecar relocates the .<file>.meta.yaml companion, if present. A missing
// sidecar is the common case and not an error; any other stat failure (a
// permission problem, an over-long name, an I/O error) is reported so the caller
// can warn, because silently leaving a sidecar behind loses everything Kagaz
// learned about the document.
func (e *Engine) moveSidecar(src, dst string) error {
	srcSide := SidecarPath(src)
	if _, err := os.Stat(srcSide); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	dstSide := SidecarPath(dst)
	if err := CopyFile(srcSide, dstSide); err != nil {
		return err
	}
	return e.stage(srcSide)
}

// sidecarSuffix mirrors sidecar.Suffix. It is duplicated rather than imported
// to keep the move engine free of a dependency on the sidecar package;
// move_test.go asserts the two never drift apart.
const sidecarSuffix = ".meta.yaml"

// SidecarPath is the sidecar companion path for a document.
func SidecarPath(doc string) string {
	return filepath.Join(filepath.Dir(doc), "."+filepath.Base(doc)+sidecarSuffix)
}

// stage renames a file into the staging area, preserving enough of its original
// location to be recognisable. Kagaz never calls Remove on a user document.
func (e *Engine) stage(path string) error {
	if e.StagingDir == "" {
		return errors.New("staging directory is not configured")
	}
	stamp := e.now().Format("20060102-150405")
	target := filepath.Join(e.StagingDir, stamp, filepath.Base(path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	target = uniquePath(target)

	if err := os.Rename(path, target); err == nil {
		return nil
	}
	// Cross-device: the staging area is on another volume. Copy, verify, and
	// only then release the original path.
	sum, err := SHA256(path)
	if err != nil {
		return err
	}
	if err := CopyFile(path, target); err != nil {
		return err
	}
	got, err := SHA256(target)
	if err != nil {
		return err
	}
	if got != sum {
		_ = os.Remove(target)
		return fmt.Errorf("checksum mismatch staging %s", path)
	}
	return os.Remove(path)
}

// resolveCollision applies the collision policy to dst. A destination counts as
// occupied if it exists on disk *or* an earlier op in the same batch has already
// claimed it; claimed maps such a destination to the SHA256 of the source bound
// for it, and may be nil for a single-op resolution.
func (e *Engine) resolveCollision(src, dst string, claimed map[string]string) (string, bool, error) {
	_, statErr := os.Stat(dst)
	onDisk := statErr == nil
	claimedSum, byBatch := claimed[dst]
	if !onDisk && !byBatch {
		return dst, false, nil
	}
	switch e.OnCollision {
	case CollisionSkip:
		return dst, true, nil
	case CollisionSuffix:
		return uniquePathExcluding(dst, claimed), false, nil
	default:
		// An identical file already at the destination is not a conflict worth
		// failing on; report it as a skip. The same holds for an identical file
		// an earlier op in this batch is about to put there.
		if a, err := SHA256(src); err == nil {
			if byBatch {
				if a == claimedSum {
					return dst, true, nil
				}
			} else if b, err2 := SHA256(dst); err2 == nil && a == b {
				return dst, true, nil
			}
		}
		return "", false, fmt.Errorf("%w: %s", ErrDestinationExists, dst)
	}
}

// uniquePath appends _2, _3, … until the path is free on disk.
func uniquePath(path string) string {
	return uniquePathExcluding(path, nil)
}

// uniquePathExcluding is uniquePath, additionally treating any path in claimed
// as occupied. Callers planning a batch pass the destinations already spoken
// for, which the filesystem cannot yet see.
func uniquePathExcluding(path string, claimed map[string]string) string {
	if !pathTaken(path, claimed) {
		return path
	}
	dir := filepath.Dir(path)
	stem, tail := splitName(filepath.Base(path))
	for i := 2; i < 1000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, i, tail))
		if !pathTaken(candidate, claimed) {
			return candidate
		}
	}
	return path
}

func pathTaken(path string, claimed map[string]string) bool {
	if _, ok := claimed[path]; ok {
		return true
	}
	_, err := os.Stat(path)
	return err == nil
}

// splitName splits a base name into the part a uniqueness counter is appended
// to and the tail that must survive intact.
//
// For an ordinary file that is stem and extension. For a sidecar it is the
// sidecar's *document* stem and everything after it: ".statement.pdf.meta.yaml"
// splits as ".statement" + ".pdf.meta.yaml", so the deduplicated name is
// ".statement_2.pdf.meta.yaml" — still recognisable as the sidecar of
// "statement_2.pdf". Appending the counter to the extension instead would
// produce ".statement.pdf.meta_2.yaml", which nothing can identify as a sidecar
// afterwards.
func splitName(base string) (stem, tail string) {
	if strings.HasPrefix(base, ".") && strings.HasSuffix(base, sidecarSuffix) && len(base) > len(sidecarSuffix)+1 {
		doc := strings.TrimSuffix(strings.TrimPrefix(base, "."), sidecarSuffix)
		docStem, docTail := splitName(doc)
		return "." + docStem, docTail + sidecarSuffix
	}
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext), ext
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// writeManifest records the planned rows before any file is touched.
func (e *Engine) writeManifest(opName string, rows []Row) (string, error) {
	if err := os.MkdirAll(e.ManifestDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%s.csv", e.now().Format("20060102-150405"), opName)
	path := uniquePath(filepath.Join(e.ManifestDir, name))

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"current_path", "original_path", "sha256"}); err != nil {
		return "", err
	}
	for _, r := range rows {
		if err := w.Write([]string{r.CurrentPath, r.OriginalPath, r.SHA256}); err != nil {
			return "", err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return path, f.Sync()
}

// ReadManifest loads a manifest CSV.
func ReadManifest(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%s: empty manifest", path)
	}
	head := records[0]
	if len(head) != 3 || head[0] != "current_path" {
		return nil, fmt.Errorf("%s: not a kagaz manifest (unexpected header)", path)
	}
	man := &Manifest{Path: path}
	if base := filepath.Base(path); strings.Contains(base, "_") {
		man.Op = strings.TrimSuffix(base[strings.Index(base, "_")+1:], ".csv")
	}
	for i, rec := range records[1:] {
		// encoding/csv already enforces a constant field count across a file and
		// rejects a ragged row before we get here, so this is belt and braces
		// against a future reader configured with FieldsPerRecord = -1.
		if len(rec) != 3 {
			return nil, fmt.Errorf("%s line %d: expected 3 fields, got %d", path, i+2, len(rec))
		}
		man.Rows = append(man.Rows, Row{CurrentPath: rec[0], OriginalPath: rec[1], SHA256: rec[2]})
	}
	return man, nil
}

// Rollback reverses a manifest, moving each file from its current path back to
// its original one. Rows whose current path is missing are reported and
// skipped, which makes rollback safe to run twice.
func (e *Engine) Rollback(man *Manifest) (*Result, error) {
	ops := make([]Op, 0, len(man.Rows))
	res := &Result{}
	for _, row := range man.Rows {
		if _, err := os.Stat(row.CurrentPath); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: not at its post-operation path, skipping", row.CurrentPath))
			continue
		}
		if _, err := os.Stat(row.OriginalPath); err == nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: original path is occupied again, skipping", row.OriginalPath))
			continue
		}
		ops = append(ops, Op{Src: row.CurrentPath, Dst: row.OriginalPath})
	}
	if len(ops) == 0 {
		res.Manifest = &Manifest{Op: "rollback"}
		return res, nil
	}
	out, err := e.Execute("rollback", ops)
	if out == nil {
		// Execute failed during planning. The rows already skipped above are the
		// most useful thing the caller can be told, so never drop them.
		out = &Result{Manifest: &Manifest{Op: "rollback"}}
	}
	out.Warnings = append(res.Warnings, out.Warnings...)
	return out, err
}

// SHA256 is the content hash of a file, lowercase hex.
func SHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CopyFile copies bytes from src to dst, preserving mode and modification time.
// It writes to a temporary file in the destination directory and renames into
// place, so an interrupted copy never leaves a half-written document.
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	st, err := in.Stat()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".kagaz-copy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, st.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chtimes(tmpName, time.Now(), st.ModTime()); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}
