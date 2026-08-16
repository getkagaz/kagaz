package move

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/sidecar"
	"github.com/getkagaz/kagaz/internal/vaultkit/tags"
)

// fixedClock is the single instant every test runs at. Tests must never depend
// on wall-clock ordering: the staging and manifest layouts are derived from
// this stamp, so "two moves in the same second" is the default, not a race.
var fixedClock = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

const fixedStamp = "20240102-030405"

// vault is the scratch layout every test works in.
type vault struct {
	t        *testing.T
	root     string
	src      string
	dst      string
	manifest string
	staging  string
	eng      *Engine
}

func newVault(t *testing.T) *vault {
	t.Helper()
	root := t.TempDir()
	v := &vault{
		t:        t,
		root:     root,
		src:      filepath.Join(root, "inbox"),
		dst:      filepath.Join(root, "vault"),
		manifest: filepath.Join(root, ".kagaz", "manifests"),
		staging:  filepath.Join(root, ".kagaz", "staging"),
	}
	for _, d := range []string{v.src, v.dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	v.eng = New(v.manifest, v.staging)
	v.eng.Now = func() time.Time { return fixedClock }
	return v
}

// write creates a file under root with the given relative path and content.
func (v *vault) write(rel, content string) string {
	v.t.Helper()
	p := filepath.Join(v.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		v.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		v.t.Fatalf("write %s: %v", rel, err)
	}
	return p
}

func (v *vault) path(rel string) string { return filepath.Join(v.root, rel) }

// stagedPath is where stage() puts a file for the fixed clock.
func (v *vault) stagedPath(base string) string {
	return filepath.Join(v.staging, fixedStamp, base)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s should not exist (%s)", path, why)
	}
}

func mustExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s should exist (%s): %v", path, why, err)
	}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// installAfterCopyHook wires the production test seam and guarantees it is
// removed again, so no other test can observe it.
func installAfterCopyHook(t *testing.T, fn func(dst string)) {
	t.Helper()
	afterCopyHook = fn
	t.Cleanup(func() { afterCopyHook = nil })
}

// ---------------------------------------------------------------------------
// Invariant 1: nothing is ever unlinked. A completed move must leave the
// original bytes recoverable from staging.
// ---------------------------------------------------------------------------

func TestExecuteStagesSourceInsteadOfDeletingIt(t *testing.T) {
	v := newVault(t)
	const body = "original bytes that must survive the move"
	src := v.write("inbox/statement.pdf", body)
	dst := v.path("vault/2024/statement.pdf")

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Fatalf("Moved = %d ops, want 1", len(res.Moved))
	}

	if got := readFile(t, dst); got != body {
		t.Errorf("destination content = %q, want %q", got, body)
	}
	mustNotExist(t, src, "the source path is vacated by the move")

	staged := v.stagedPath("statement.pdf")
	mustExist(t, staged, "the source is renamed into staging, never removed")
	if got := readFile(t, staged); got != body {
		t.Errorf("staged content = %q, want the original bytes %q", got, body)
	}
}

func TestStageRequiresConfiguredStagingDir(t *testing.T) {
	v := newVault(t)
	v.eng.StagingDir = ""
	src := v.write("inbox/a.txt", "a")

	_, err := v.eng.Execute("organise", []Op{{Src: src, Dst: v.path("vault/a.txt")}})
	if err == nil {
		t.Fatal("Execute with no staging directory should fail rather than unlink the source")
	}
	if !strings.Contains(err.Error(), "staging directory is not configured") {
		t.Errorf("error = %v, want it to name the missing staging directory", err)
	}
	// The engine copied to the destination but could not stage; the source must
	// still be on disk. Nothing is ever unlinked.
	mustExist(t, src, "a staging failure must not cost the user the source file")
}

// ---------------------------------------------------------------------------
// Invariant 2: the manifest is written before any file is touched, and is
// complete even when a later file in the batch fails.
// ---------------------------------------------------------------------------

func TestManifestIsCompleteWhenALaterFileFails(t *testing.T) {
	v := newVault(t)
	good := v.write("inbox/good.txt", "good")
	bad := v.write("inbox/bad.txt", "bad")

	// A regular file where the second destination needs a directory: MkdirAll
	// inside moveOne fails, so the second op fails after the first succeeded.
	v.write("vault/blocked", "not a directory")
	goodDst := v.path("vault/ok/good.txt")
	badDst := v.path("vault/blocked/bad.txt")

	res, err := v.eng.Execute("organise", []Op{
		{Src: good, Dst: goodDst},
		{Src: bad, Dst: badDst},
	})
	if err == nil {
		t.Fatal("Execute should report the failed second op")
	}
	if res == nil || res.Manifest == nil {
		t.Fatal("a failed Execute must still return the manifest, or the crash is not resumable")
	}
	manPath := res.Manifest.Path
	if manPath == "" {
		t.Fatal("manifest path is empty; there is no resumable record on disk")
	}
	mustExist(t, manPath, "the manifest is written before the first byte moves")

	// The on-disk manifest must describe the WHOLE batch, including the op that
	// never ran, otherwise a crash loses the record of what was in flight.
	onDisk, rerr := ReadManifest(manPath)
	if rerr != nil {
		t.Fatalf("ReadManifest(%s): %v", manPath, rerr)
	}
	if len(onDisk.Rows) != 2 {
		t.Fatalf("manifest has %d rows, want 2 (both the moved and the failed file)", len(onDisk.Rows))
	}
	wantRows := map[string]string{
		goodDst: hashOf("good"),
		badDst:  hashOf("bad"),
	}
	for _, r := range onDisk.Rows {
		want, ok := wantRows[r.CurrentPath]
		if !ok {
			t.Errorf("unexpected manifest row for %s", r.CurrentPath)
			continue
		}
		if r.SHA256 != want {
			t.Errorf("row %s sha256 = %s, want %s", r.CurrentPath, r.SHA256, want)
		}
		delete(wantRows, r.CurrentPath)
	}
	for missing := range wantRows {
		t.Errorf("manifest is missing a row for %s", missing)
	}

	// The first file really did move, and its source is recoverable.
	mustExist(t, goodDst, "the first op completed")
	mustExist(t, v.stagedPath("good.txt"), "the first source was staged")
	// The failed file was never touched.
	mustExist(t, bad, "a failed op leaves its source untouched")
	mustNotExist(t, v.stagedPath("bad.txt"), "a failed op stages nothing")
}

func TestManifestExistsBeforeAnyFileIsTouched(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/a.txt", "a")
	dst := v.path("vault/a.txt")

	// The hook runs after the destination is written but before verification,
	// which is the earliest point at which a file has been touched at all.
	var manifestsAtFirstTouch []string
	installAfterCopyHook(t, func(string) {
		if manifestsAtFirstTouch != nil {
			return
		}
		entries, err := os.ReadDir(v.manifest)
		if err != nil {
			t.Errorf("manifest dir unreadable at first touch: %v", err)
			manifestsAtFirstTouch = []string{}
			return
		}
		manifestsAtFirstTouch = []string{}
		for _, e := range entries {
			manifestsAtFirstTouch = append(manifestsAtFirstTouch, e.Name())
		}
	})

	if _, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(manifestsAtFirstTouch) != 1 {
		t.Fatalf("manifests present when the first byte was written = %v, want exactly one", manifestsAtFirstTouch)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/a.txt", "a")
	dst := v.path("vault/a.txt")
	v.eng.DryRun = true

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Errorf("dry run should plan 1 move, got %d", len(res.Moved))
	}
	if res.Manifest.Path != "" {
		t.Errorf("dry run wrote a manifest at %s", res.Manifest.Path)
	}
	if len(res.Manifest.Rows) != 1 {
		t.Errorf("dry run manifest has %d rows, want 1", len(res.Manifest.Rows))
	}
	mustExist(t, src, "dry run must not move the source")
	mustNotExist(t, dst, "dry run must not create the destination")
	mustNotExist(t, v.manifest, "dry run must not create the manifest directory")
	mustNotExist(t, v.staging, "dry run must not create the staging directory")
}

// ---------------------------------------------------------------------------
// Invariant 3: SHA256 is verified on every copy; a mismatch aborts and leaves
// the source untouched.
// ---------------------------------------------------------------------------

func TestChecksumMismatchAbortsAndLeavesSourceUntouched(t *testing.T) {
	v := newVault(t)
	const body = "the real document"
	src := v.write("inbox/doc.txt", body)
	dst := v.path("vault/doc.txt")

	// Corrupt the destination between the copy and the verification, which is
	// exactly the failure the invariant exists to catch.
	installAfterCopyHook(t, func(d string) {
		if err := os.WriteFile(d, []byte("corrupted on the way in"), 0o644); err != nil {
			t.Errorf("corruption injection failed: %v", err)
		}
	})

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err == nil {
		t.Fatal("a corrupted copy must abort the move")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %v, want a checksum mismatch", err)
	}
	if !strings.Contains(err.Error(), "source untouched") {
		t.Errorf("error = %v, want it to tell the user the source is safe", err)
	}

	mustExist(t, src, "a checksum mismatch leaves the source untouched")
	if got := readFile(t, src); got != body {
		t.Errorf("source content = %q, want %q", got, body)
	}
	mustNotExist(t, dst, "the corrupt destination is removed")
	mustNotExist(t, v.stagedPath("doc.txt"), "a failed move stages nothing")
	if res != nil && len(res.Moved) != 0 {
		t.Errorf("Moved = %v, want nothing reported as moved", res.Moved)
	}
	// The manifest still records the intent, so the failure is diagnosable.
	if res == nil || res.Manifest == nil || res.Manifest.Path == "" {
		t.Fatal("a checksum failure must still leave a manifest behind")
	}
	man, rerr := ReadManifest(res.Manifest.Path)
	if rerr != nil {
		t.Fatalf("ReadManifest: %v", rerr)
	}
	if len(man.Rows) != 1 || man.Rows[0].SHA256 != hashOf(body) {
		t.Errorf("manifest rows = %+v, want one row hashing the original source", man.Rows)
	}
}

func TestChecksumMismatchOnSecondFileKeepsFirstMove(t *testing.T) {
	v := newVault(t)
	first := v.write("inbox/first.txt", "first")
	second := v.write("inbox/second.txt", "second")

	installAfterCopyHook(t, func(d string) {
		if filepath.Base(d) != "second.txt" {
			return
		}
		if err := os.WriteFile(d, []byte("garbage"), 0o644); err != nil {
			t.Errorf("corruption injection failed: %v", err)
		}
	})

	_, err := v.eng.Execute("organise", []Op{
		{Src: first, Dst: v.path("vault/first.txt")},
		{Src: second, Dst: v.path("vault/second.txt")},
	})
	if err == nil {
		t.Fatal("the corrupted second copy must abort")
	}
	mustExist(t, v.path("vault/first.txt"), "the first file completed before the failure")
	mustExist(t, v.stagedPath("first.txt"), "the first source is recoverable from staging")
	mustExist(t, second, "the failed source is untouched")
	if got := readFile(t, second); got != "second" {
		t.Errorf("failed source content = %q, want %q", got, "second")
	}
	mustNotExist(t, v.path("vault/second.txt"), "the corrupt destination is removed")
}

func TestSHA256MatchesKnownDigest(t *testing.T) {
	v := newVault(t)
	// SHA256("") and SHA256("abc") are the standard published vectors.
	cases := []struct {
		name, body, want string
	}{
		{"empty", "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := v.write("inbox/hash-"+tc.name, tc.body)
			got, err := SHA256(p)
			if err != nil {
				t.Fatalf("SHA256: %v", err)
			}
			if got != tc.want {
				t.Errorf("SHA256 = %s, want %s", got, tc.want)
			}
		})
	}
	if _, err := SHA256(v.path("inbox/does-not-exist")); err == nil {
		t.Error("SHA256 of a missing file should fail")
	}
}

func TestCopyFilePreservesBytesModeAndModTime(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/c.txt", "copy me")
	if err := os.Chmod(src, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	modTime := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	if err := os.Chtimes(src, modTime, modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	dst := v.path("vault/c.txt")

	if err := CopyFile(src, dst); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if got := readFile(t, dst); got != "copy me" {
		t.Errorf("content = %q", got)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", st.Mode().Perm())
	}
	if !st.ModTime().Equal(modTime) {
		t.Errorf("modtime = %v, want %v", st.ModTime(), modTime)
	}
	// No temporary file is left behind in the destination directory.
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".kagaz-copy-") {
			t.Errorf("CopyFile left a temporary file behind: %s", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Invariant 4: collision policies behave exactly as documented.
// ---------------------------------------------------------------------------

func TestCollisionPolicies(t *testing.T) {
	cases := []struct {
		name string
		// existing maps a destination-relative path to its content.
		existing  map[string]string
		policy    Collision
		srcBody   string
		wantErr   error
		wantDst   string // relative path the source is expected to land at, "" = nowhere
		wantSkip  bool
		wantMoved bool
	}{
		{
			name:      "fail: free destination moves normally",
			policy:    CollisionFail,
			srcBody:   "new",
			wantDst:   "vault/a.txt",
			wantMoved: true,
		},
		{
			name:     "fail: different file at destination is an error",
			existing: map[string]string{"vault/a.txt": "something else"},
			policy:   CollisionFail,
			srcBody:  "new",
			wantErr:  ErrDestinationExists,
		},
		{
			name:     "fail: byte-identical file at destination is a skip, not a failure",
			existing: map[string]string{"vault/a.txt": "identical"},
			policy:   CollisionFail,
			srcBody:  "identical",
			wantSkip: true,
		},
		{
			name:      "suffix: first collision becomes _2",
			existing:  map[string]string{"vault/a.txt": "other"},
			policy:    CollisionSuffix,
			srcBody:   "new",
			wantDst:   "vault/a_2.txt",
			wantMoved: true,
		},
		{
			name:      "suffix: second collision becomes _3",
			existing:  map[string]string{"vault/a.txt": "other", "vault/a_2.txt": "other2"},
			policy:    CollisionSuffix,
			srcBody:   "new",
			wantDst:   "vault/a_3.txt",
			wantMoved: true,
		},
		{
			name:      "suffix: free destination keeps its name",
			policy:    CollisionSuffix,
			srcBody:   "new",
			wantDst:   "vault/a.txt",
			wantMoved: true,
		},
		{
			name:     "skip: leaves the source alone",
			existing: map[string]string{"vault/a.txt": "other"},
			policy:   CollisionSkip,
			srcBody:  "new",
			wantSkip: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newVault(t)
			v.eng.OnCollision = tc.policy
			for rel, body := range tc.existing {
				v.write(rel, body)
			}
			src := v.write("inbox/a.txt", tc.srcBody)
			dst := v.path("vault/a.txt")

			res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				mustExist(t, src, "a collision failure leaves the source where it is")
				for rel, body := range tc.existing {
					if got := readFile(t, v.path(rel)); got != body {
						t.Errorf("%s = %q, want the untouched %q", rel, got, body)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			if tc.wantSkip {
				if len(res.Skipped) != 1 {
					t.Fatalf("Skipped = %v, want exactly one skip", res.Skipped)
				}
				if len(res.Moved) != 0 {
					t.Errorf("Moved = %v, want nothing moved", res.Moved)
				}
				mustExist(t, src, "a skip leaves the source alone")
				if got := readFile(t, src); got != tc.srcBody {
					t.Errorf("source = %q, want %q", got, tc.srcBody)
				}
				if len(res.Manifest.Rows) != 0 {
					t.Errorf("a skip should produce no manifest rows, got %+v", res.Manifest.Rows)
				}
				return
			}

			if tc.wantMoved {
				if len(res.Moved) != 1 {
					t.Fatalf("Moved = %v, want exactly one move", res.Moved)
				}
				want := v.path(tc.wantDst)
				if res.Moved[0].Dst != want {
					t.Errorf("destination = %s, want %s", res.Moved[0].Dst, want)
				}
				if got := readFile(t, want); got != tc.srcBody {
					t.Errorf("%s = %q, want %q", tc.wantDst, got, tc.srcBody)
				}
				// Pre-existing files are never overwritten.
				for rel, body := range tc.existing {
					if got := readFile(t, v.path(rel)); got != body {
						t.Errorf("%s = %q, want the untouched %q", rel, got, body)
					}
				}
				mustExist(t, v.stagedPath("a.txt"), "the source is staged, not deleted")
			}
		})
	}
}

// Two ops in one batch aiming at the same destination are the case the
// filesystem cannot see while planning: nothing has moved yet, so the second op
// would otherwise find a free destination. All three policies are affected, so
// all three are pinned here.
//
// (These tests were originally written to FAIL, documenting a data-loss defect:
// the second copy silently overwrote the first document and the first manifest
// row kept a hash the file no longer had, so a rollback restored the wrong
// bytes. They now assert the fixed behaviour.)

func TestCollisionSuffixWithinOneBatch(t *testing.T) {
	v := newVault(t)
	v.eng.OnCollision = CollisionSuffix
	a := v.write("inbox/a/report.txt", "contents of A")
	b := v.write("inbox/b/report.txt", "contents of B")
	dst := v.path("vault/report.txt")

	res, err := v.eng.Execute("organise", []Op{{Src: a, Dst: dst}, {Src: b, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Moved) != 2 {
		t.Fatalf("Moved = %v, want both files moved", res.Moved)
	}
	if res.Moved[0].Dst == res.Moved[1].Dst {
		t.Fatalf("both ops in one batch resolved to the same destination %s; "+
			"CollisionSuffix must append _2 for the second, or the first document is overwritten",
			res.Moved[0].Dst)
	}
	if got, _ := os.ReadFile(dst); string(got) != "contents of A" {
		t.Errorf("%s = %q, want %q; the first document must survive", dst, string(got), "contents of A")
	}
	if got, _ := os.ReadFile(v.path("vault/report_2.txt")); string(got) != "contents of B" {
		t.Errorf("vault/report_2.txt = %q, want %q", string(got), "contents of B")
	}
	// Every manifest row must describe the file that is actually on disk,
	// otherwise rollback restores the wrong bytes.
	assertManifestMatchesDisk(t, res.Manifest)

	// The real proof: a rollback must return each document to its own origin.
	if _, err := v.eng.Rollback(res.Manifest); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := readFile(t, a); got != "contents of A" {
		t.Errorf("rollback restored %q to A's original path, want %q", got, "contents of A")
	}
	if got := readFile(t, b); got != "contents of B" {
		t.Errorf("rollback restored %q to B's original path, want %q", got, "contents of B")
	}
}

func TestCollisionFailWithinOneBatch(t *testing.T) {
	v := newVault(t)
	v.eng.OnCollision = CollisionFail
	a := v.write("inbox/a/report.txt", "contents of A")
	b := v.write("inbox/b/report.txt", "contents of B")
	dst := v.path("vault/report.txt")

	_, err := v.eng.Execute("organise", []Op{{Src: a, Dst: dst}, {Src: b, Dst: dst}})
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("error = %v, want ErrDestinationExists: two ops claiming one destination is exactly "+
			"the conflict CollisionFail refuses to resolve for the user", err)
	}
	// The conflict is detected while planning, so nothing moved at all.
	mustExist(t, a, "a planning failure moves nothing")
	mustExist(t, b, "a planning failure moves nothing")
	mustNotExist(t, dst, "a planning failure creates no destination")
	mustNotExist(t, v.manifest, "a planning failure writes no manifest")
}

func TestCollisionFailWithinOneBatchSkipsIdenticalSources(t *testing.T) {
	v := newVault(t)
	v.eng.OnCollision = CollisionFail
	a := v.write("inbox/a/report.txt", "identical contents")
	b := v.write("inbox/b/report.txt", "identical contents")
	dst := v.path("vault/report.txt")

	res, err := v.eng.Execute("organise", []Op{{Src: a, Dst: dst}, {Src: b, Dst: dst}})
	if err != nil {
		t.Fatalf("two byte-identical sources are a skip, not a failure: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Fatalf("Moved = %v, want exactly one move", res.Moved)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("Skipped = %v, want exactly one skip", res.Skipped)
	}
	if got := readFile(t, dst); got != "identical contents" {
		t.Errorf("%s = %q", dst, got)
	}
	mustExist(t, b, "the skipped source stays where it is")
	if len(res.Manifest.Rows) != 1 {
		t.Errorf("manifest rows = %+v, want only the row that moved", res.Manifest.Rows)
	}
	assertManifestMatchesDisk(t, res.Manifest)
}

func TestCollisionSkipWithinOneBatch(t *testing.T) {
	v := newVault(t)
	v.eng.OnCollision = CollisionSkip
	a := v.write("inbox/a/report.txt", "contents of A")
	b := v.write("inbox/b/report.txt", "contents of B")
	dst := v.path("vault/report.txt")

	res, err := v.eng.Execute("organise", []Op{{Src: a, Dst: dst}, {Src: b, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Fatalf("Moved = %v, want exactly one move", res.Moved)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("Skipped = %v, want the second op skipped", res.Skipped)
	}
	if got := readFile(t, dst); got != "contents of A" {
		t.Errorf("%s = %q, want %q", dst, got, "contents of A")
	}
	mustExist(t, b, "CollisionSkip leaves the source alone")
	if got := readFile(t, b); got != "contents of B" {
		t.Errorf("skipped source = %q, want it untouched", got)
	}
	assertManifestMatchesDisk(t, res.Manifest)
}

func TestThreeWayCollisionWithinOneBatchWalksTheSuffixes(t *testing.T) {
	v := newVault(t)
	v.eng.OnCollision = CollisionSuffix
	// One file is already at the destination, then three ops in one batch all
	// aim at it: the counter must continue past the on-disk file.
	v.write("vault/report.txt", "already here")
	dst := v.path("vault/report.txt")
	var ops []Op
	for _, name := range []string{"a", "b", "c"} {
		ops = append(ops, Op{Src: v.write("inbox/"+name+"/report.txt", "contents of "+name), Dst: dst})
	}

	res, err := v.eng.Execute("organise", ops)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got []string
	for _, m := range res.Moved {
		got = append(got, filepath.Base(m.Dst))
	}
	want := []string{"report_2.txt", "report_3.txt", "report_4.txt"}
	if len(got) != len(want) {
		t.Fatalf("destinations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("destinations = %v, want %v", got, want)
		}
	}
	if body := readFile(t, dst); body != "already here" {
		t.Errorf("the pre-existing file was overwritten: %q", body)
	}
	assertManifestMatchesDisk(t, res.Manifest)
}

// assertManifestMatchesDisk is the property that makes rollback trustworthy:
// every row must name a file that exists and hash to what the row claims.
func assertManifestMatchesDisk(t *testing.T, man *Manifest) {
	t.Helper()
	for _, row := range man.Rows {
		sum, err := SHA256(row.CurrentPath)
		if err != nil {
			t.Errorf("manifest row points at %s which does not exist: %v", row.CurrentPath, err)
			continue
		}
		if sum != row.SHA256 {
			t.Errorf("manifest row for %s records sha %s but the file on disk hashes to %s; "+
				"a rollback would restore the wrong document",
				row.CurrentPath, row.SHA256[:12], sum[:12])
		}
	}
}

// ---------------------------------------------------------------------------
// Invariant 5: rollback reverses a manifest, is safe to run twice, and skips
// unrestorable rows with a warning rather than an error.
// ---------------------------------------------------------------------------

func TestRollbackRestoresOriginalPaths(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/a.txt", "alpha")
	dst := v.path("vault/2024/a.txt")

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	back, err := v.eng.Rollback(res.Manifest)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(back.Moved) != 1 {
		t.Fatalf("rollback moved %v, want one file", back.Moved)
	}
	mustExist(t, src, "rollback puts the document back at its original path")
	if got := readFile(t, src); got != "alpha" {
		t.Errorf("restored content = %q, want %q", got, "alpha")
	}
	mustNotExist(t, dst, "rollback vacates the post-operation path")
}

func TestRollbackIsSafeToRunTwice(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/a.txt", "alpha")
	dst := v.path("vault/a.txt")

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := v.eng.Rollback(res.Manifest); err != nil {
		t.Fatalf("first Rollback: %v", err)
	}
	before := readFile(t, src)

	second, err := v.eng.Rollback(res.Manifest)
	if err != nil {
		t.Fatalf("second Rollback must not be an error: %v", err)
	}
	if len(second.Moved) != 0 {
		t.Errorf("second rollback moved %v, want nothing", second.Moved)
	}
	if len(second.Warnings) != 1 {
		t.Fatalf("second rollback warnings = %v, want exactly one", second.Warnings)
	}
	if !strings.Contains(second.Warnings[0], dst) {
		t.Errorf("warning %q should name the missing path %s", second.Warnings[0], dst)
	}
	if got := readFile(t, src); got != before {
		t.Errorf("second rollback changed the restored file: %q -> %q", before, got)
	}
}

func TestRollbackSkipsUnrestorableRows(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(v *vault, src, dst string)
		wantSubstr string
	}{
		{
			name:       "current path missing",
			mutate:     func(v *vault, _, dst string) { os.Remove(dst) },
			wantSubstr: "not at its post-operation path",
		},
		{
			name: "original path occupied again",
			mutate: func(v *vault, src, _ string) {
				v.write("inbox/a.txt", "a different file took the name back")
			},
			wantSubstr: "original path is occupied again",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newVault(t)
			src := v.write("inbox/a.txt", "alpha")
			dst := v.path("vault/a.txt")
			res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			tc.mutate(v, src, dst)

			back, err := v.eng.Rollback(res.Manifest)
			if err != nil {
				t.Fatalf("Rollback must warn, not fail: %v", err)
			}
			if len(back.Moved) != 0 {
				t.Errorf("Moved = %v, want nothing restored", back.Moved)
			}
			if len(back.Warnings) != 1 {
				t.Fatalf("Warnings = %v, want exactly one", back.Warnings)
			}
			if !strings.Contains(back.Warnings[0], tc.wantSubstr) {
				t.Errorf("warning %q, want it to mention %q", back.Warnings[0], tc.wantSubstr)
			}
		})
	}
}

func TestRollbackOfEmptyManifest(t *testing.T) {
	v := newVault(t)
	res, err := v.eng.Rollback(&Manifest{Op: "organise"})
	if err != nil {
		t.Fatalf("Rollback of an empty manifest: %v", err)
	}
	if res.Manifest == nil || res.Manifest.Op != "rollback" {
		t.Errorf("Manifest = %+v, want an empty rollback manifest", res.Manifest)
	}
	if len(res.Moved) != 0 || len(res.Warnings) != 0 {
		t.Errorf("Moved = %v, Warnings = %v, want both empty", res.Moved, res.Warnings)
	}
}

func TestRollbackRoundTripsThroughDisk(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/a.txt", "alpha")
	dst := v.path("vault/a.txt")
	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Rollback is normally driven by a manifest read back from disk, not by the
	// in-memory Result, so exercise that path too.
	onDisk, err := ReadManifest(res.Manifest.Path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if _, err := v.eng.Rollback(onDisk); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := readFile(t, src); got != "alpha" {
		t.Errorf("restored content = %q, want %q", got, "alpha")
	}
}

// ---------------------------------------------------------------------------
// Invariant 6: sidecars travel with their document; a missing sidecar is the
// normal case, not an error.
// ---------------------------------------------------------------------------

func TestSidecarPathAgreesWithSidecarPackage(t *testing.T) {
	for _, doc := range []string{
		"/vault/2024/statement.pdf",
		"/vault/no-extension",
		"/vault/dotted.name.v2.pdf",
		"relative.txt",
	} {
		if got, want := SidecarPath(doc), sidecar.Path(doc); got != want {
			t.Errorf("SidecarPath(%q) = %q, sidecar.Path = %q; the two must never drift apart", doc, got, want)
		}
	}
}

func TestSidecarTravelsWithItsDocument(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/statement.pdf", "%PDF fake")
	if err := sidecar.Write(src, &sidecar.Meta{
		DocType:   "bank-statement",
		SourceSHA: hashOf("%PDF fake"),
		Text:      "extracted text",
	}); err != nil {
		t.Fatalf("sidecar.Write: %v", err)
	}
	dst := v.path("vault/2024/statement.pdf")

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none for a sidecar that moved cleanly", res.Warnings)
	}

	moved, err := sidecar.Read(dst)
	if err != nil {
		t.Fatalf("sidecar.Read(dst): %v", err)
	}
	if moved == nil {
		t.Fatal("the sidecar did not travel with its document")
	}
	if moved.DocType != "bank-statement" || moved.Text != "extracted text" {
		t.Errorf("moved sidecar = %+v, want the original contents", moved)
	}
	mustNotExist(t, SidecarPath(src), "the old sidecar path is vacated")
	mustExist(t, v.stagedPath(filepath.Base(SidecarPath(src))), "the old sidecar is staged, not deleted")
}

func TestMissingSidecarIsNotAnError(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/plain.txt", "no sidecar here")
	dst := v.path("vault/plain.txt")

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none: most of the vault has no sidecar", res.Warnings)
	}
	if len(res.Moved) != 1 {
		t.Errorf("Moved = %v, want one move", res.Moved)
	}
	mustNotExist(t, SidecarPath(dst), "no sidecar should be invented")
}

func TestSidecarIsNotMovedTwiceOnRollback(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/doc.txt", "body")
	if err := sidecar.Write(src, &sidecar.Meta{DocType: "letter", SourceSHA: hashOf("body")}); err != nil {
		t.Fatalf("sidecar.Write: %v", err)
	}
	dst := v.path("vault/doc.txt")

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := v.eng.Rollback(res.Manifest); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	back, err := sidecar.Read(src)
	if err != nil {
		t.Fatalf("sidecar.Read after rollback: %v", err)
	}
	if back == nil || back.DocType != "letter" {
		t.Errorf("sidecar after rollback = %+v, want it back beside its document", back)
	}
}

// ---------------------------------------------------------------------------
// Invariant 7: Finder tags are carried across, and tags.ErrUnsupported degrades
// to a warning (never a failure). On Linux CI xattrs in the com.apple.* namespace
// are unsupported, which is exactly the degradation path this test walks.
// ---------------------------------------------------------------------------

// xattrsWork reports whether this filesystem can actually store Finder tags.
// macOS: yes. Linux CI: no (foreign xattr namespace), which is the point.
func xattrsWork(t *testing.T, probe string) bool {
	t.Helper()
	err := tags.Write(probe, []string{"kagaz-probe"})
	if err == nil {
		if werr := tags.Write(probe, nil); werr != nil && !errors.Is(werr, tags.ErrUnsupported) {
			t.Fatalf("clearing probe tags: %v", werr)
		}
		return true
	}
	if !errors.Is(err, tags.ErrUnsupported) {
		// Any other write failure is still "this filesystem cannot hold Finder
		// tags"; the point of the test is that the move survives either way.
		t.Logf("xattr probe failed with %v; treating this filesystem as tag-less", err)
	}
	return false
}

func TestTagsAreCarriedAcrossOrDegradeGracefully(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/tagged.txt", "tagged body")
	dst := v.path("vault/tagged.txt")

	probe := v.write("inbox/.probe", "p")
	supported := xattrsWork(t, probe)

	want := []string{"invoice", "2024"}
	if supported {
		if err := tags.Write(src, want); err != nil {
			t.Fatalf("tags.Write: %v", err)
		}
	}

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	// Platform-independent assertions: the move succeeds and reports no
	// warnings either way. tags.Copy swallows ErrUnsupported, so a filesystem
	// without xattrs can never turn a move into a failure.
	if err != nil {
		t.Fatalf("Execute must not fail over Finder tags (xattr support: %v): %v", supported, err)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "Finder tags") {
			t.Errorf("tag transfer produced warning %q; unsupported xattrs must degrade silently, "+
				"and a supported filesystem must simply work", w)
		}
	}
	if len(res.Moved) != 1 {
		t.Fatalf("Moved = %v, want one move", res.Moved)
	}

	if !supported {
		t.Log("xattrs unsupported on this filesystem (the Linux CI path): asserting only that the move succeeded")
		return
	}
	// macOS-only from here: the tags must actually have travelled.
	got, err := tags.Read(dst)
	if err != nil {
		t.Fatalf("tags.Read(dst): %v", err)
	}
	sort.Strings(got)
	wantSorted := tags.Normalize(want)
	if len(got) != len(wantSorted) {
		t.Fatalf("tags on destination = %v, want %v", got, wantSorted)
	}
	for i := range got {
		if got[i] != wantSorted[i] {
			t.Errorf("tags on destination = %v, want %v", got, wantSorted)
			break
		}
	}
}

func TestUntaggedFileMovesWithoutWarnings(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/plain.txt", "plain")
	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: v.path("vault/plain.txt")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", res.Warnings)
	}
}

// ---------------------------------------------------------------------------
// Invariant 8: ReadManifest rejects a file that is not a Kagaz manifest and
// round-trips one that is.
// ---------------------------------------------------------------------------

func TestReadManifestRoundTrip(t *testing.T) {
	v := newVault(t)
	rows := []Row{
		{CurrentPath: "/vault/2024/a.pdf", OriginalPath: "/inbox/a.pdf", SHA256: hashOf("a")},
		{CurrentPath: "/vault/2024/b, with comma.pdf", OriginalPath: "/inbox/b.pdf", SHA256: hashOf("b")},
		{CurrentPath: "/vault/2024/c \"quoted\".pdf", OriginalPath: "/inbox/c.pdf", SHA256: hashOf("c")},
	}
	path, err := v.eng.writeManifest("organise", rows)
	if err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	man, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if man.Op != "organise" {
		t.Errorf("Op = %q, want %q", man.Op, "organise")
	}
	if man.Path != path {
		t.Errorf("Path = %q, want %q", man.Path, path)
	}
	if len(man.Rows) != len(rows) {
		t.Fatalf("Rows = %d, want %d", len(man.Rows), len(rows))
	}
	for i, r := range man.Rows {
		if r != rows[i] {
			t.Errorf("row %d = %+v, want %+v", i, r, rows[i])
		}
	}
}

func TestReadManifestRejectsNonManifests(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		wantSubstr string
	}{
		{"empty file", "", "empty manifest"},
		{"plain prose", "this is just a note to self\n", "not a kagaz manifest"},
		{"wrong header", "path,from,hash\n/a,/b,c\n", "not a kagaz manifest"},
		{"too few header columns", "current_path,original_path\n/a,/b\n", "not a kagaz manifest"},
		// encoding/csv enforces a constant field count itself, so this is
		// rejected before ReadManifest's own len(rec) != 3 check is reached.
		// That check is therefore unreachable; see the task report.
		{"right header wrong row width", "current_path,original_path,sha256\n/a,/b\n", "wrong number of fields"},
		{"yaml sidecar", "doctype: invoice\nsource_sha256: abc\n", "not a kagaz manifest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "20240102-030405_organise.csv")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := ReadManifest(p)
			if err == nil {
				t.Fatalf("ReadManifest accepted %q as a manifest", tc.content)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantSubstr)
			}
		})
	}

	if _, err := ReadManifest(filepath.Join(t.TempDir(), "absent.csv")); err == nil {
		t.Error("ReadManifest of a missing file should fail")
	}
}

func TestReadManifestHeaderOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "20240102-030405_organise.csv")
	if err := os.WriteFile(p, []byte("current_path,original_path,sha256\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	man, err := ReadManifest(p)
	if err != nil {
		t.Fatalf("a header-only manifest is valid (an operation that moved nothing): %v", err)
	}
	if len(man.Rows) != 0 {
		t.Errorf("Rows = %+v, want none", man.Rows)
	}
	if man.Op != "organise" {
		t.Errorf("Op = %q, want %q", man.Op, "organise")
	}
}

// ---------------------------------------------------------------------------
// Invariant 9: uniquePath and the staging timestamp layout do not collide when
// two moves happen in the same second.
// ---------------------------------------------------------------------------

func TestUniquePathProperties(t *testing.T) {
	dir := t.TempDir()

	// Property: a free path is returned unchanged, whatever the name looks like.
	for _, name := range []string{
		"a.txt", "no-extension", ".dotfile", "dotted.name.v2.pdf",
		"spaces in name.txt", "UPPER.TXT", "unicode-ünïcødé.pdf",
	} {
		p := filepath.Join(dir, name)
		if got := uniquePath(p); got != p {
			t.Errorf("uniquePath(%q) = %q, want it unchanged when the path is free", p, got)
		}
	}

	// Property: an occupied path yields a free path in the same directory with
	// the same extension, and never the input itself.
	for _, name := range []string{"a.txt", "no-extension", "dotted.name.v2.pdf", "spaces in name.txt"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		got := uniquePath(p)
		if got == p {
			t.Errorf("uniquePath(%q) returned the occupied path", p)
			continue
		}
		if filepath.Dir(got) != filepath.Dir(p) {
			t.Errorf("uniquePath(%q) = %q, changed directory", p, got)
		}
		if filepath.Ext(got) != filepath.Ext(p) {
			t.Errorf("uniquePath(%q) = %q, changed extension", p, got)
		}
		if _, err := os.Stat(got); err == nil {
			t.Errorf("uniquePath(%q) = %q, which already exists", p, got)
		}
	}
}

func TestUniquePathCountsUpwards(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "report.pdf")
	want := []string{"report.pdf", "report_2.pdf", "report_3.pdf", "report_4.pdf"}
	for _, w := range want {
		got := uniquePath(base)
		if filepath.Base(got) != w {
			t.Fatalf("uniquePath = %q, want %q", filepath.Base(got), w)
		}
		if err := os.WriteFile(got, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func TestTwoMovesInTheSameSecondDoNotCollide(t *testing.T) {
	v := newVault(t)
	// Same clock for both operations, same base name: the staging layout and the
	// manifest name must both stay distinct without any wall-clock help.
	first := v.write("inbox/one/report.pdf", "first document")
	second := v.write("inbox/two/report.pdf", "second document")

	res1, err := v.eng.Execute("organise", []Op{{Src: first, Dst: v.path("vault/a/report.pdf")}})
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	res2, err := v.eng.Execute("organise", []Op{{Src: second, Dst: v.path("vault/b/report.pdf")}})
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}

	if res1.Manifest.Path == res2.Manifest.Path {
		t.Fatalf("both manifests were written to %s; the second overwrote the first", res1.Manifest.Path)
	}
	for _, m := range []*Manifest{res1.Manifest, res2.Manifest} {
		on, rerr := ReadManifest(m.Path)
		if rerr != nil {
			t.Fatalf("ReadManifest(%s): %v", m.Path, rerr)
		}
		if len(on.Rows) != 1 {
			t.Errorf("%s has %d rows, want 1", m.Path, len(on.Rows))
		}
	}

	// Both sources are in the same second's staging directory under distinct
	// names, and both sets of original bytes are recoverable.
	stampDir := filepath.Join(v.staging, fixedStamp)
	entries, err := os.ReadDir(stampDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("staging holds %d files, want 2 distinct entries", len(entries))
	}
	bodies := map[string]bool{}
	for _, e := range entries {
		bodies[readFile(t, filepath.Join(stampDir, e.Name()))] = true
	}
	for _, want := range []string{"first document", "second document"} {
		if !bodies[want] {
			t.Errorf("staging lost %q; recovered bodies were %v", want, bodies)
		}
	}
}

// ---------------------------------------------------------------------------
// Planning-time validation.
// ---------------------------------------------------------------------------

func TestExecuteRejectsBadInput(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(v *vault) []Op
		wantSubstr string
	}{
		{
			name: "missing source",
			setup: func(v *vault) []Op {
				return []Op{{Src: v.path("inbox/absent.txt"), Dst: v.path("vault/absent.txt")}}
			},
			wantSubstr: "source",
		},
		{
			name: "source is a directory",
			setup: func(v *vault) []Op {
				d := filepath.Join(v.root, "inbox", "folder")
				if err := os.MkdirAll(d, 0o755); err != nil {
					v.t.Fatalf("mkdir: %v", err)
				}
				return []Op{{Src: d, Dst: v.path("vault/folder")}}
			},
			wantSubstr: "kagaz moves files",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newVault(t)
			_, err := v.eng.Execute("organise", tc.setup(v))
			if err == nil {
				t.Fatal("Execute should have refused this op")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantSubstr)
			}
			mustNotExist(t, v.manifest, "planning failures must not write a manifest")
		})
	}
}

func TestExecuteWithNoOps(t *testing.T) {
	v := newVault(t)
	res, err := v.eng.Execute("organise", nil)
	if err != nil {
		t.Fatalf("Execute(nil): %v", err)
	}
	if res.Manifest == nil || res.Manifest.Op != "organise" {
		t.Errorf("Manifest = %+v, want an empty manifest named for the op", res.Manifest)
	}
	if len(res.Moved) != 0 || len(res.Skipped) != 0 {
		t.Errorf("Moved = %v, Skipped = %v, want both empty", res.Moved, res.Skipped)
	}
	mustNotExist(t, v.manifest, "an empty batch writes no manifest")
}

func TestExecuteSkipsSourceEqualToDestination(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/a.txt", "same place")

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: src}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("Skipped = %v, want one skip", res.Skipped)
	}
	if len(res.Moved) != 0 {
		t.Errorf("Moved = %v, want nothing moved", res.Moved)
	}
	if got := readFile(t, src); got != "same place" {
		t.Errorf("source = %q, want it untouched", got)
	}
	mustNotExist(t, v.manifest, "a no-op batch writes no manifest")
}

func TestExecuteResolvesRelativePaths(t *testing.T) {
	v := newVault(t)
	v.write("inbox/rel.txt", "relative")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(v.root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("chdir back: %v", err)
		}
	})

	res, err := v.eng.Execute("organise", []Op{{Src: "inbox/rel.txt", Dst: "vault/rel.txt"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Fatalf("Moved = %v, want one move", res.Moved)
	}
	if !filepath.IsAbs(res.Moved[0].Src) || !filepath.IsAbs(res.Moved[0].Dst) {
		t.Errorf("Moved = %+v, want absolute paths recorded", res.Moved[0])
	}
	for _, row := range res.Manifest.Rows {
		if !filepath.IsAbs(row.CurrentPath) || !filepath.IsAbs(row.OriginalPath) {
			t.Errorf("manifest row %+v must hold absolute paths or rollback breaks when the cwd changes", row)
		}
	}
}

// ---------------------------------------------------------------------------
// Regression tests for the defects fixed after the first pass of this suite.
// ---------------------------------------------------------------------------

func TestSplitNameKeepsTheSidecarTailIntact(t *testing.T) {
	cases := []struct{ base, stem, tail string }{
		{"report.pdf", "report", ".pdf"},
		{"no-extension", "no-extension", ""},
		{"dotted.name.v2.pdf", "dotted.name.v2", ".pdf"},
		{".statement.pdf.meta.yaml", ".statement", ".pdf.meta.yaml"},
		{".no-extension.meta.yaml", ".no-extension", ".meta.yaml"},
		{".dotted.name.v2.pdf.meta.yaml", ".dotted.name.v2", ".pdf.meta.yaml"},
	}
	for _, tc := range cases {
		stem, tail := splitName(tc.base)
		if stem != tc.stem || tail != tc.tail {
			t.Errorf("splitName(%q) = (%q, %q), want (%q, %q)", tc.base, stem, tail, tc.stem, tc.tail)
		}
		if stem+tail != tc.base {
			t.Errorf("splitName(%q) is lossy: %q + %q", tc.base, stem, tail)
		}
	}
}

func TestStagedSidecarKeepsItsSidecarIdentity(t *testing.T) {
	v := newVault(t)
	// Two documents with the same name and the same fixed clock: the second
	// pair collides in staging, so both the document and its sidecar are
	// deduplicated. The staged sidecar must still be recognisable as one.
	for _, sub := range []string{"one", "two"} {
		src := v.write("inbox/"+sub+"/statement.pdf", "body "+sub)
		if err := sidecar.Write(src, &sidecar.Meta{DocType: "bank-statement", SourceSHA: hashOf("body " + sub)}); err != nil {
			t.Fatalf("sidecar.Write: %v", err)
		}
		res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: v.path("vault/" + sub + "/statement.pdf")}})
		if err != nil {
			t.Fatalf("Execute(%s): %v", sub, err)
		}
		if len(res.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none", res.Warnings)
		}
	}

	stampDir := filepath.Join(v.staging, fixedStamp)
	entries, err := os.ReadDir(stampDir)
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	var sidecars, docs []string
	for _, e := range entries {
		if sidecar.IsSidecar(e.Name()) {
			sidecars = append(sidecars, e.Name())
			continue
		}
		docs = append(docs, e.Name())
	}
	sort.Strings(sidecars)
	sort.Strings(docs)
	if len(entries) != 4 {
		t.Fatalf("staging holds %v, want two documents and two sidecars", entries)
	}
	wantSidecars := []string{".statement.pdf.meta.yaml", ".statement_2.pdf.meta.yaml"}
	if len(sidecars) != 2 || sidecars[0] != wantSidecars[0] || sidecars[1] != wantSidecars[1] {
		t.Errorf("staged sidecars = %v, want %v; a deduplicated sidecar must keep its .meta.yaml tail "+
			"or nothing can identify it later", sidecars, wantSidecars)
	}
	// The staged sidecar names its staged document, so the pair can be matched up.
	for i, s := range sidecars {
		doc := filepath.Base(sidecar.DocumentFor(filepath.Join(stampDir, s)))
		if i < len(docs) && doc != docs[i] {
			t.Errorf("staged sidecar %s points at document %s, but staging holds %v", s, doc, docs)
		}
	}
}

func TestSidecarThatCannotBeCopiedWarnsButDoesNotFailTheMove(t *testing.T) {
	v := newVault(t)
	src := v.write("inbox/doc.txt", "body")
	// A directory where the sidecar should be: it exists, so it is not the
	// "no sidecar" case, but it cannot be copied.
	if err := os.MkdirAll(SidecarPath(src), 0o755); err != nil {
		t.Fatalf("mkdir sidecar path: %v", err)
	}
	dst := v.path("vault/doc.txt")

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: dst}})
	if err != nil {
		t.Fatalf("a sidecar problem must not fail the move: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Fatalf("Moved = %v, want the document to have moved", res.Moved)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly one about the sidecar", res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "sidecar not moved") {
		t.Errorf("warning = %q, want it to say the sidecar was not moved", res.Warnings[0])
	}
	if got := readFile(t, dst); got != "body" {
		t.Errorf("destination = %q, want the document to have arrived intact", got)
	}
}

func TestUnstatableSidecarWarnsRatherThanVanishingSilently(t *testing.T) {
	v := newVault(t)
	// A document name long enough that its sidecar name exceeds NAME_MAX: the
	// stat fails with something other than "does not exist", which must be
	// reported rather than mistaken for "this document has no sidecar".
	long := strings.Repeat("n", 250) + ".txt"
	src := v.write("inbox/"+long, "body")
	if _, err := os.Stat(SidecarPath(src)); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Skipf("this filesystem accepts the over-long sidecar name (%v); nothing to test", err)
	}

	res, err := v.eng.Execute("organise", []Op{{Src: src, Dst: v.path("vault/" + long)}})
	if err != nil {
		t.Fatalf("a sidecar stat failure must not fail the move: %v", err)
	}
	if len(res.Moved) != 1 {
		t.Fatalf("Moved = %v, want the document to have moved", res.Moved)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "sidecar not moved") {
		t.Errorf("Warnings = %v, want one warning naming the sidecar; a stat error that is not "+
			"ErrNotExist must never be mistaken for the no-sidecar case", res.Warnings)
	}
}

func TestRollbackKeepsItsWarningsWhenExecuteFailsWhilePlanning(t *testing.T) {
	v := newVault(t)
	// Row 1 is unrestorable and produces a warning. Row 2 makes Execute fail
	// during planning (a directory is not a movable file), so Execute returns a
	// nil Result. The warning from row 1 must survive that.
	missing := v.path("vault/gone.txt")
	dir := v.path("vault/adirectory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	man := &Manifest{Op: "organise", Rows: []Row{
		{CurrentPath: missing, OriginalPath: v.path("inbox/gone.txt"), SHA256: hashOf("gone")},
		{CurrentPath: dir, OriginalPath: v.path("inbox/adirectory"), SHA256: hashOf("dir")},
	}}

	res, err := v.eng.Rollback(man)
	if err == nil {
		t.Fatal("Rollback should surface the planning failure")
	}
	if res == nil {
		t.Fatal("Rollback must still return a Result, or the warnings it collected are lost")
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want the skipped row's warning to survive the failure", res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], missing) {
		t.Errorf("warning = %q, want it to name %s", res.Warnings[0], missing)
	}
}
