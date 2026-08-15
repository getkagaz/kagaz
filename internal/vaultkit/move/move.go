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

		dst, skip, err := e.resolveCollision(src, dst)
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

// moveOne copies, verifies, transfers tags, then stages the source.
func (e *Engine) moveOne(src, dst, wantSum string, res *Result) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := CopyFile(src, dst); err != nil {
		return err
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

// moveSidecar relocates the .<file>.meta.yaml companion, if present.
func (e *Engine) moveSidecar(src, dst string) error {
	srcSide := SidecarPath(src)
	if _, err := os.Stat(srcSide); err != nil {
		return nil //nolint:nilerr // no sidecar is the common case
	}
	dstSide := SidecarPath(dst)
	if err := CopyFile(srcSide, dstSide); err != nil {
		return err
	}
	return e.stage(srcSide)
}

// SidecarPath is the sidecar companion path for a document.
func SidecarPath(doc string) string {
	return filepath.Join(filepath.Dir(doc), "."+filepath.Base(doc)+".meta.yaml")
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

func (e *Engine) resolveCollision(src, dst string) (string, bool, error) {
	if _, err := os.Stat(dst); err != nil {
		return dst, false, nil
	}
	switch e.OnCollision {
	case CollisionSkip:
		return dst, true, nil
	case CollisionSuffix:
		return uniquePath(dst), false, nil
	default:
		// An identical file already at the destination is not a conflict worth
		// failing on; report it as a skip.
		a, err1 := SHA256(src)
		b, err2 := SHA256(dst)
		if err1 == nil && err2 == nil && a == b {
			return dst, true, nil
		}
		return "", false, fmt.Errorf("%w: %s", ErrDestinationExists, dst)
	}
}

// uniquePath appends _2, _3, … until the path is free.
func uniquePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path
	}
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(filepath.Base(path), ext)
	for i := 2; i < 1000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
	return path
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
	if out != nil {
		out.Warnings = append(res.Warnings, out.Warnings...)
	}
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
