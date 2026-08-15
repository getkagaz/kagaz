package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Host is the pinned download host. It is a constant, not a setting: a
// configurable download host in the one component allowed to leave the machine
// is a configuration change away from being an exfiltration channel.
const Host = "huggingface.co"

// DefaultEndpoint is the pinned base URL for the Hugging Face hub.
const DefaultEndpoint = "https://" + Host

// DefaultRevision is the revision requested when the caller names none.
//
// It is a branch name, which is *not* by itself reproducible -- so Pull never
// downloads from a branch. It resolves the branch to a concrete commit sha
// first, downloads only from that sha, and records it in the manifest, which
// is what makes a later `--revision <sha>` reproduce today's bytes exactly.
const DefaultRevision = "main"

// pinnedRevisions maps a repo to a commit sha that this build is pinned to.
//
// It is empty on purpose. A pin is only worth having if the sha is a real
// commit that someone verified against the hub; inventing one here would make
// every pull of that repo fail with a confusing 404, and copying one in
// unverified would be worse. Until a maintainer adds a verified entry, Pull
// resolves DefaultRevision to a concrete sha at pull time, records it, and
// tells the user the sha to pass to `--revision` to pin it themselves.
var pinnedRevisions = map[string]string{}

// PinnedRevision returns the build-pinned revision for repo, if there is one.
func PinnedRevision(repo string) (string, bool) {
	rev, ok := pinnedRevisions[repo]
	return rev, ok
}

// maxFileSize caps a single downloaded file. MLX 4-bit weights for the default
// model are a couple of gigabytes; 32 GiB is far above any legitimate shard and
// still bounds what a lying or compromised server can make Kagaz write to disk.
const maxFileSize = 32 << 30

// maxMetadataSize caps an API response body. Metadata is tens of kilobytes.
const maxMetadataSize = 8 << 20

// maxRedirects bounds the redirect chain for an LFS download.
const maxRedirects = 5

// wantedFiles is the set of files an MLX text model needs. Everything else in
// a repo -- READMEs, images, .bin/.pt duplicates of the same weights, ONNX
// exports -- is skipped, which keeps a pull to what the helper actually loads.
var wantedFiles = map[string]bool{
	"config.json":                  true,
	"generation_config.json":       true,
	"tokenizer.json":               true,
	"tokenizer_config.json":        true,
	"tokenizer.model":              true,
	"special_tokens_map.json":      true,
	"vocab.json":                   true,
	"merges.txt":                   true,
	"added_tokens.json":            true,
	"chat_template.jinja":          true,
	"model.safetensors.index.json": true,
}

// wantedSuffixes are the file extensions kept regardless of exact name, so a
// sharded model (model-00001-of-00002.safetensors) comes down in full.
var wantedSuffixes = []string{".safetensors"}

// Options configures one Pull.
type Options struct {
	// Repo is the Hugging Face repo id, e.g.
	// "mlx-community/Qwen2.5-3B-Instruct-4bit".
	Repo string
	// Revision is a commit sha, tag or branch. Empty means the build pin for
	// Repo if there is one, else DefaultRevision.
	Revision string
	// Force re-downloads and re-verifies even when the model is already ready.
	Force bool
	// Log receives human-readable progress lines, including the informational
	// license note. Nil discards them.
	Log func(string)
	// Progress, when set, is called with per-file byte counts during a
	// download. total is -1 when the server did not advertise a length.
	Progress func(file string, done, total int64)
}

// Client performs the pull. The zero value is usable and pinned to the public
// Hugging Face hub.
type Client struct {
	// Store is the destination cache. The zero value uses DefaultRoot.
	Store Store

	// endpoint is the base URL. Unexported and only overridden by tests: a
	// caller-settable endpoint would defeat the point of pinning the host.
	endpoint string
	// hostAllowed decides whether a redirect target is acceptable. Nil means
	// hubHostAllowed.
	hostAllowed func(*url.URL) error
	// httpClient is the transport. Nil means a client built by newHTTPClient.
	httpClient *http.Client
	// now is a clock seam for tests.
	now func() time.Time
}

// Result reports what a Pull did.
type Result struct {
	// Repo and Revision identify exactly what is now on disk.
	Repo     string
	Revision string
	// Dir is the model directory.
	Dir string
	// Manifest is the manifest as written.
	Manifest *Manifest
	// Downloaded lists files fetched (or resumed) by this run.
	Downloaded []string
	// Reused lists files already present and verified.
	Reused []string
	// AlreadyReady is true when the model was ready and nothing was fetched.
	AlreadyReady bool
}

// Pull downloads repo's weights into the cache, resuming an interrupted
// download rather than restarting it, and marks the model ready only once
// every file's SHA256 has been verified against the bytes on disk.
//
// Re-running against a ready model is a no-op unless Options.Force is set.
func (c *Client) Pull(ctx context.Context, opts Options) (*Result, error) {
	if err := ValidateRepo(opts.Repo); err != nil {
		return nil, err
	}
	logf := opts.logger()

	// Informational only. Global rule from docs/model-use.md: Kagaz
	// distributes no weights, so it is a downstream user rather than a
	// distributor, and has no business adjudicating a model's license on the
	// user's behalf. The note is printed; nothing waits on it.
	logf(LicenseNote(opts.Repo))

	dir, err := c.Store.Dir(opts.Repo)
	if err != nil {
		return nil, err
	}

	if !opts.Force {
		ready, man, err := c.Store.Ready(opts.Repo)
		if err != nil {
			return nil, err
		}
		if ready {
			logf(fmt.Sprintf("%s is already downloaded and verified (revision %s); nothing to do", opts.Repo, man.Revision))
			return &Result{Repo: opts.Repo, Revision: man.Revision, Dir: dir, Manifest: man, AlreadyReady: true}, nil
		}
	}

	revision := opts.Revision
	if revision == "" {
		if pinned, ok := PinnedRevision(opts.Repo); ok {
			revision = pinned
		} else {
			revision = DefaultRevision
		}
	}

	info, err := c.repoInfo(ctx, opts.Repo, revision)
	if err != nil {
		return nil, err
	}
	resolved := strings.TrimSpace(info.SHA)
	if resolved == "" {
		// Without a concrete commit the download is not reproducible, and the
		// manifest would record a promise it cannot keep.
		return nil, fmt.Errorf("models: %s: the hub did not report a commit for revision %q", opts.Repo, revision)
	}
	if resolved != revision {
		logf(fmt.Sprintf("revision %s resolves to commit %s (pass --revision %s to pin it)", revision, resolved, resolved))
	}

	files, err := selectFiles(opts.Repo, info)
	if err != nil {
		return nil, err
	}

	// Carry forward whatever the last attempt verified, so a resumed pull does
	// not re-hash gigabytes it already checked.
	prev, err := c.Store.ReadManifest(opts.Repo)
	if err != nil {
		// A manifest we cannot read is not a reason to refuse a fresh pull.
		prev = nil
	}
	verified := map[string]File{}
	if prev != nil && prev.Revision == resolved {
		for _, f := range prev.Files {
			if f.Verified {
				verified[f.Name] = f
			}
		}
	} else if prev != nil && prev.Revision != "" {
		// The user is pulling a different commit than the interrupted run was.
		// Any leftover `.part` holds revision A's bytes, and resuming it would
		// splice revision B onto that prefix -- a file that is the right length
		// and wrong content. Discard them before starting.
		if err := discardPartials(dir); err != nil {
			return nil, err
		}
		logf(fmt.Sprintf("previous pull was of revision %s; discarding its partial downloads", prev.Revision))
	}

	man := &Manifest{
		Repo:     opts.Repo,
		Revision: resolved,
		Endpoint: c.baseURL(),
		Status:   StatusDownloading,
		Files:    make([]File, 0, len(files)),
	}
	for _, f := range files {
		man.Files = append(man.Files, File{Name: f.Name})
	}
	// The manifest lands before the first byte does, mirroring move.Engine:
	// an interrupted pull must leave a record that says what it was doing.
	if err := c.Store.WriteManifest(opts.Repo, man); err != nil {
		return nil, err
	}

	res := &Result{Repo: opts.Repo, Revision: resolved, Dir: dir}
	for i, want := range files {
		target := filepath.Join(dir, filepath.FromSlash(want.Name))

		if have, ok := verified[want.Name]; ok {
			if st, err := os.Stat(target); err == nil && st.Size() == have.Size {
				man.Files[i] = have
				res.Reused = append(res.Reused, want.Name)
				continue
			}
		}

		logf(fmt.Sprintf("downloading %s (%d/%d)", want.Name, i+1, len(files)))
		got, err := c.fetch(ctx, opts, resolved, want, target)
		if err != nil {
			// The manifest stays on disk in `downloading` state: the next run
			// resumes from the .part file rather than starting over.
			return res, err
		}
		man.Files[i] = got
		res.Downloaded = append(res.Downloaded, want.Name)

		// Persist after every file so an interruption loses at most one file's
		// verification, never the whole batch.
		if err := c.Store.WriteManifest(opts.Repo, man); err != nil {
			return res, err
		}
	}

	if err := verifyComplete(man); err != nil {
		return res, err
	}
	man.Status = StatusReady
	if err := c.Store.WriteManifest(opts.Repo, man); err != nil {
		return res, err
	}
	res.Manifest = man
	logf(fmt.Sprintf("%s is ready at %s (revision %s)", opts.Repo, dir, resolved))
	return res, nil
}

// isTopLevelConfig and isTopLevelWeights are the readiness gate, and both
// insist the file sits at the top of the model directory.
//
// ModelCache.resolve in the Swift MLX helper uses contentsOfDirectory, which
// does not recurse. A nested onnx/model.safetensors would satisfy a naive
// suffix test here while the helper still answered model_not_found, so this
// package's definition of "ready" is deliberately the helper's definition of
// "present", not a looser one.
func isTopLevelConfig(name string) bool {
	return name == "config.json"
}

func isTopLevelWeights(name string) bool {
	return !strings.Contains(name, "/") && strings.HasSuffix(name, ".safetensors")
}

// discardPartials removes every `.part` file in a model directory. These are
// Kagaz's own incomplete temp files, never user documents.
func discardPartials(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// verifyComplete is the last gate before `status: ready`. Every file must
// carry a verified digest, and the directory must satisfy what the Swift
// helper checks for, so "ready" and "loadable" never disagree.
func verifyComplete(man *Manifest) error {
	hasConfig, hasWeights := false, false
	for _, f := range man.Files {
		if !f.Verified || f.SHA256 == "" {
			return fmt.Errorf("models: %s: %s was not verified; refusing to mark the download ready", man.Repo, f.Name)
		}
		if isTopLevelConfig(f.Name) {
			hasConfig = true
		}
		if isTopLevelWeights(f.Name) {
			hasWeights = true
		}
	}
	if !hasConfig || !hasWeights {
		return fmt.Errorf("models: %s: downloaded set has no top-level config.json and/or no top-level .safetensors weights; refusing to mark it ready", man.Repo)
	}
	return nil
}

// remoteFile is one file the hub says exists at a revision.
type remoteFile struct {
	Name string
	Size int64
	// SHA256 is the hub's advertised LFS digest, when it publishes one. Weight
	// shards are LFS objects and do have it; small config files do not.
	SHA256 string
}

// repoInfoResponse is the subset of the hub's model API this needs.
type repoInfoResponse struct {
	SHA      string `json:"sha"`
	Siblings []struct {
		Name string `json:"rfilename"`
		Size int64  `json:"size"`
		LFS  *struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	} `json:"siblings"`
}

// repoInfo resolves a revision to a commit and lists the repo's files.
func (c *Client) repoInfo(ctx context.Context, repo, revision string) (*repoInfoResponse, error) {
	if strings.ContainsAny(revision, "/?#") || strings.TrimSpace(revision) != revision || revision == "" {
		return nil, fmt.Errorf("models: %q is not a valid revision", revision)
	}
	u := c.baseURL() + "/api/models/" + repo + "/revision/" + url.PathEscape(revision) + "?blobs=true"
	resp, err := c.get(ctx, u, 0)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize))
	if err != nil {
		return nil, fmt.Errorf("models: reading repo metadata: %w", err)
	}
	var info repoInfoResponse
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("models: %s: unreadable repo metadata: %w", repo, err)
	}
	return &info, nil
}

// selectFiles keeps the files an MLX model actually loads, in a stable order.
func selectFiles(repo string, info *repoInfoResponse) ([]remoteFile, error) {
	var out []remoteFile
	for _, s := range info.Siblings {
		name := path.Clean(strings.TrimSpace(s.Name))
		if !wanted(name) {
			continue
		}
		// A server-supplied filename is never joined onto a path unchecked.
		if err := safeRelName(name); err != nil {
			return nil, fmt.Errorf("models: %s: %w", repo, err)
		}
		f := remoteFile{Name: name, Size: s.Size}
		if s.LFS != nil {
			f.SHA256 = strings.ToLower(strings.TrimSpace(s.LFS.OID))
			if s.LFS.Size > 0 {
				f.Size = s.LFS.Size
			}
		}
		if f.Size > maxFileSize {
			return nil, fmt.Errorf("models: %s: %s is %d bytes, above the %d byte limit", repo, name, f.Size, int64(maxFileSize))
		}
		// A file with neither a published digest nor a size cannot be verified
		// at all, and verifyFile would then mark it Verified having checked
		// nothing. Refusing here makes the hub contract fail safe: if the
		// metadata is not good enough to prove a download is correct, the pull
		// stops rather than writing `status: ready` over unverified bytes.
		if f.SHA256 == "" && f.Size <= 0 {
			return nil, fmt.Errorf("models: %s: the hub published neither a size nor a sha256 for %s, "+
				"so the download could not be verified; refusing the pull", repo, name)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	hasConfig, hasWeights := false, false
	for _, f := range out {
		if isTopLevelConfig(f.Name) {
			hasConfig = true
		}
		if isTopLevelWeights(f.Name) {
			hasWeights = true
		}
	}
	if !hasConfig || !hasWeights {
		return nil, fmt.Errorf("models: %s does not look like an MLX model: it has no top-level config.json and/or no top-level .safetensors weights", repo)
	}
	return out, nil
}

// wanted reports whether a repo file is part of an MLX model.
//
// Nested paths are excluded, and that exclusion is load-bearing rather than
// tidiness. ModelCache.resolve in the Swift helper checks for config.json and
// a .safetensors file with contentsOfDirectory, which lists the *top level
// only*. A repo that also publishes onnx/model.safetensors would otherwise
// satisfy this package's readiness check while the helper still reported
// model_not_found -- the precise "the pull succeeded but classification says
// the model is missing" failure this cache exists to prevent.
func wanted(name string) bool {
	if strings.Contains(name, "/") {
		return false
	}
	if wantedFiles[name] {
		return true
	}
	for _, suffix := range wantedSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	// Sharded index files are named after the weights they index.
	return strings.HasSuffix(name, ".safetensors.index.json")
}

// safeRelName rejects any server-supplied name that would escape the model
// directory or collide with the manifest.
func safeRelName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("refusing file with empty or relative name %q", name)
	case strings.HasPrefix(name, "/"), strings.HasPrefix(name, "../"), strings.Contains(name, "/../"), strings.HasSuffix(name, "/.."):
		return fmt.Errorf("refusing file with a traversing name %q", name)
	case strings.Contains(name, `\`), strings.ContainsRune(name, 0):
		return fmt.Errorf("refusing file with an invalid name %q", name)
	case path.Base(name) == ManifestName:
		return fmt.Errorf("refusing file that would overwrite the download manifest")
	}
	return nil
}

// fetch downloads one file into target, resuming from a partial `.part` file
// when one is there, then hashes the result and verifies it.
func (c *Client) fetch(ctx context.Context, opts Options, revision string, want remoteFile, target string) (File, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return File{}, err
	}
	part := target + ".part"

	// A complete file already on disk (from an earlier run whose manifest was
	// lost) is verified rather than re-downloaded.
	if st, err := os.Stat(target); err == nil && st.Mode().IsRegular() {
		f, err := verifyFile(target, want)
		if err == nil {
			return f, nil
		}
		// It did not match: fall through and fetch it again. The bad copy is
		// replaced by the rename below, never left half-trusted.
	}

	offset := int64(0)
	if st, err := os.Stat(part); err == nil && st.Mode().IsRegular() {
		offset = st.Size()
		switch {
		case want.SHA256 == "":
			// Without a published digest, a resumed file can only ever be
			// size-checked, and a `.part` left by a different revision can be
			// exactly the right size. These files are the small ones
			// (config.json, tokenizer_config.json, merges.txt), so restarting
			// costs nothing and removes the splice risk entirely.
			offset = 0
		case want.Size > 0 && offset > want.Size:
			// A partial file longer than the whole file is garbage, not a
			// resume point.
			offset = 0
		}
	}

	resp, err := c.get(ctx, c.fileURL(opts.Repo, revision, want.Name), offset)
	if err != nil {
		if errors.Is(err, errRangeNotSatisfiable) && offset > 0 {
			// The server says the range starts past the end: the .part file is
			// already the whole file. Fall through to verification, which is
			// what decides whether that is true.
			resp = nil
		} else {
			return File{}, err
		}
	}

	if resp != nil {
		// Only a 206 means the server honoured the range. A 200 is a full body
		// and must overwrite, never append -- appending a full body onto a
		// partial file is how a "resumable" downloader silently corrupts.
		resumed := offset > 0 && resp.StatusCode == http.StatusPartialContent
		body := resp.Body
		flags := os.O_CREATE | os.O_WRONLY
		if resumed {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
			offset = 0
		}
		f, err := os.OpenFile(part, flags, 0o644)
		if err != nil {
			body.Close()
			return File{}, err
		}
		limit := int64(maxFileSize)
		if want.Size > 0 {
			// One byte of slack so an over-long body is detected as a size
			// mismatch rather than silently truncated to the right length.
			limit = want.Size - offset + 1
		}
		w := &progressWriter{
			w:     f,
			done:  offset,
			total: want.Size,
			name:  want.Name,
			fn:    opts.Progress,
			now:   c.clock(),
		}
		_, copyErr := io.Copy(w, io.LimitReader(body, limit))
		body.Close()
		syncErr := f.Sync()
		closeErr := f.Close()
		if copyErr != nil {
			return File{}, fmt.Errorf("models: downloading %s: %w", want.Name, copyErr)
		}
		if syncErr != nil {
			return File{}, syncErr
		}
		if closeErr != nil {
			return File{}, closeErr
		}
	}

	got, err := verifyFile(part, want)
	if err != nil {
		// The partial file is now known to be wrong, and leaving it would wedge
		// every future pull: at offset == want.Size the server answers 416, no
		// bytes are written, verification fails again, and the user has to find
		// and delete a dotless `.part` file by hand to recover. Removing it
		// makes a re-run the recovery path. It is Kagaz's own temp file, not a
		// user document, so Global Constraint 3 does not apply.
		if rmErr := os.Remove(part); rmErr != nil && !os.IsNotExist(rmErr) {
			return File{}, fmt.Errorf("%w (and the partial file %s could not be removed: %v)", err, part, rmErr)
		}
		return File{}, fmt.Errorf("%w (the partial download was discarded; re-run the pull)", err)
	}
	if err := os.Rename(part, target); err != nil {
		return File{}, err
	}
	return got, nil
}

// verifyFile hashes path and checks it against what the hub advertised. The
// digest is always recorded; it is additionally *compared* whenever the hub
// published one, and a mismatch is a hard failure that leaves nothing usable
// behind.
//
// selectFiles guarantees every remoteFile reaching here has a positive size, a
// published digest, or both, so this never returns Verified having checked
// nothing. The belt-and-braces check below enforces that invariant locally
// rather than trusting a caller to keep it.
func verifyFile(path string, want remoteFile) (File, error) {
	if want.SHA256 == "" && want.Size <= 0 {
		return File{}, fmt.Errorf("models: %s: nothing to verify against (no published size or sha256); refusing to mark it verified", want.Name)
	}
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return File{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))

	if want.Size > 0 && st.Size() != want.Size {
		return File{}, fmt.Errorf("models: %s: got %d bytes, expected %d; download incomplete", want.Name, st.Size(), want.Size)
	}
	if want.SHA256 != "" && sum != want.SHA256 {
		return File{}, fmt.Errorf("models: %s: sha256 %s does not match the published %s; refusing the file", want.Name, sum[:12], want.SHA256[:12])
	}
	return File{Name: want.Name, Size: st.Size(), SHA256: sum, Verified: true}, nil
}

// fileURL is the hub's resolve URL for one file at one commit. Every path
// segment is escaped here; the URL is built by Kagaz from a validated repo id,
// a resolved commit and a name that passed safeRelName, never handed over by
// the server as a ready-made link.
func (c *Client) fileURL(repo, revision, name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return c.baseURL() + "/" + repo + "/resolve/" + url.PathEscape(revision) + "/" + strings.Join(segs, "/")
}

// progressWriter counts bytes and reports them at most a few times a second,
// which is enough for a progress bar and cheap enough to sit in the copy loop.
type progressWriter struct {
	w      io.Writer
	done   int64
	total  int64
	name   string
	fn     func(string, int64, int64)
	now    func() time.Time
	lastAt time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.fn != nil {
		now := p.now()
		if now.Sub(p.lastAt) >= 200*time.Millisecond {
			p.lastAt = now
			p.fn(p.name, p.done, p.total)
		}
	}
	return n, err
}

func (o Options) logger() func(string) {
	if o.Log == nil {
		return func(string) {}
	}
	return o.Log
}

func (c *Client) clock() func() time.Time {
	if c.now != nil {
		return c.now
	}
	return time.Now
}
