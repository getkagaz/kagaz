package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeHub is a stand-in for the Hugging Face hub. Every test in this file runs
// against it; nothing here ever touches the real network.
type fakeHub struct {
	repo string
	sha  string

	mu sync.Mutex
	// files is the repo content, plus entries that should be advertised but
	// not downloaded (README.md and friends).
	files map[string][]byte
	// oidOverride replaces the advertised LFS digest for a file, to exercise
	// the mismatch path.
	oidOverride map[string]string
	// truncateAfter cuts a file's body short to simulate an interrupted
	// download, once, per file name.
	truncateAfter map[string]int
	// ignoreRange makes the server answer 200 with the whole body even when a
	// Range was asked for -- a real and legal server behaviour that a naive
	// resumer corrupts.
	ignoreRange bool
	// extraSiblings are advertised without existing, e.g. a traversing name.
	extraSiblings []map[string]any

	requests   []string
	rangeAsked map[string]int64
}

func newFakeHub(t *testing.T, repo string, files map[string][]byte) *fakeHub {
	t.Helper()
	return &fakeHub{
		repo:          repo,
		sha:           "0123456789abcdef0123456789abcdef01234567",
		files:         files,
		oidOverride:   map[string]string{},
		truncateAfter: map[string]int{},
		rangeAsked:    map[string]int64{},
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, r.URL.Path)
	h.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/api/models/"):
		h.serveMetadata(w, r)
	case strings.Contains(r.URL.Path, "/resolve/"):
		h.serveFile(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *fakeHub) serveMetadata(w http.ResponseWriter, r *http.Request) {
	want := "/api/models/" + h.repo + "/revision/"
	if !strings.HasPrefix(r.URL.Path, want) {
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	siblings := make([]map[string]any, 0, len(h.files))
	names := make([]string, 0, len(h.files))
	for name := range h.files {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		body := h.files[name]
		oid := sha256hex(body)
		if o, ok := h.oidOverride[name]; ok {
			oid = o
		}
		siblings = append(siblings, map[string]any{
			"rfilename": name,
			"size":      len(body),
			"lfs":       map[string]any{"oid": oid, "size": len(body)},
		})
	}
	siblings = append(siblings, h.extraSiblings...)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sha": h.sha, "siblings": siblings})
}

func (h *fakeHub) serveFile(w http.ResponseWriter, r *http.Request) {
	i := strings.Index(r.URL.Path, "/resolve/")
	rest := r.URL.Path[i+len("/resolve/"):]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		http.NotFound(w, r)
		return
	}
	rev, name := rest[:slash], rest[slash+1:]
	name, _ = url.PathUnescape(name)
	if rev != h.sha {
		// Downloading from anything but the resolved commit is a bug worth
		// failing loudly on: it is what makes a pull reproducible.
		http.Error(w, "downloads must use the resolved commit, got "+rev, http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	body, ok := h.files[name]
	truncate, hasTruncate := h.truncateAfter[name]
	if hasTruncate {
		delete(h.truncateAfter, name) // one interruption per file
	}
	ignoreRange := h.ignoreRange
	h.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	offset := int64(0)
	if rng := r.Header.Get("Range"); rng != "" && !ignoreRange {
		var err error
		offset, err = parseRangeStart(rng)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.rangeAsked[name] = offset
		h.mu.Unlock()
		if offset >= int64(len(body)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(body)-1, len(body)))
	}

	payload := body[offset:]
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	if offset > 0 {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if hasTruncate && truncate < len(payload) {
		// Fewer bytes than the declared Content-Length: the client sees the
		// connection die mid-file, exactly as an interrupted download does.
		_, _ = w.Write(payload[:truncate])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(payload)
}

func parseRangeStart(rng string) (int64, error) {
	if !strings.HasPrefix(rng, "bytes=") {
		return 0, fmt.Errorf("bad range %q", rng)
	}
	spec := strings.TrimPrefix(rng, "bytes=")
	spec = strings.TrimSuffix(spec, "-")
	return strconv.ParseInt(spec, 10, 64)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// testClient wires a Client to a fake hub, relaxing only the host policy --
// the strictness of the real policy is asserted separately in
// TestDefaultHostPolicy.
func testClient(t *testing.T, hub *fakeHub) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	return &Client{
		Store:       Store{Root: t.TempDir()},
		endpoint:    srv.URL,
		hostAllowed: func(u *url.URL) error { return nil },
	}, srv
}

func modelFiles() map[string][]byte {
	return map[string][]byte{
		"config.json":       []byte(`{"model_type":"qwen2","quantization":{"bits":4}}`),
		"tokenizer.json":    []byte(`{"version":"1.0","model":{}}`),
		"model.safetensors": []byte(strings.Repeat("weights-", 4096)),
		"README.md":         []byte("# not part of the model"),
		"pytorch_model.bin": []byte("torch duplicate that must not be downloaded"),
	}
}

func TestPullDownloadsVerifiesAndMarksReady(t *testing.T) {
	hub := newFakeHub(t, "org/model", modelFiles())
	c, _ := testClient(t, hub)

	var logged []string
	res, err := c.Pull(context.Background(), Options{
		Repo: "org/model",
		Log:  func(s string) { logged = append(logged, s) },
	})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Revision != hub.sha {
		t.Fatalf("Revision = %q, want the resolved commit %q", res.Revision, hub.sha)
	}
	if res.Manifest.Status != StatusReady {
		t.Fatalf("Status = %q, want %q", res.Manifest.Status, StatusReady)
	}

	// The informational licence note is printed, and printed before anything
	// is downloaded.
	if len(logged) == 0 || !strings.Contains(logged[0], "License") {
		t.Fatalf("first log line was not the licence note: %q", logged)
	}

	dir, _ := c.Store.Dir("org/model")
	for name, body := range modelFiles() {
		path := filepath.Join(dir, name)
		_, err := os.Stat(path)
		switch name {
		case "README.md", "pytorch_model.bin":
			if err == nil {
				t.Errorf("%s was downloaded; only MLX model files should be", name)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != string(body) {
			t.Errorf("%s: content mismatch", name)
		}
	}

	// Every manifest entry carries a real digest of the bytes on disk.
	for _, f := range res.Manifest.Files {
		if !f.Verified || len(f.SHA256) != 64 {
			t.Errorf("manifest entry %+v is not verified with a sha256", f)
		}
		if f.SHA256 != sha256hex(modelFiles()[f.Name]) {
			t.Errorf("%s: recorded sha256 does not match the content", f.Name)
		}
	}

	ready, _, err := c.Store.Ready("org/model")
	if err != nil || !ready {
		t.Fatalf("Ready = %v, %v; want true", ready, err)
	}

	// No .part files survive a successful pull.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("leftover partial file %s", e.Name())
		}
	}
}

func TestPullIsANoOpWhenAlreadyReady(t *testing.T) {
	hub := newFakeHub(t, "org/model", modelFiles())
	c, _ := testClient(t, hub)

	if _, err := c.Pull(context.Background(), Options{Repo: "org/model"}); err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	hub.mu.Lock()
	hub.requests = nil
	hub.mu.Unlock()

	res, err := c.Pull(context.Background(), Options{Repo: "org/model"})
	if err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if !res.AlreadyReady {
		t.Fatal("second Pull did not report AlreadyReady")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.requests) != 0 {
		t.Fatalf("second Pull made %d requests: %v; a ready model must be a no-op", len(hub.requests), hub.requests)
	}
}

// TestPullResumesAfterInterruption is the resumability requirement, tested
// from the interrupted side rather than the happy one: the first pull dies
// mid-weights, and the second must resume with a Range request, not restart.
func TestPullResumesAfterInterruption(t *testing.T) {
	files := modelFiles()
	hub := newFakeHub(t, "org/model", files)
	hub.truncateAfter["model.safetensors"] = 1000
	c, _ := testClient(t, hub)

	if _, err := c.Pull(context.Background(), Options{Repo: "org/model"}); err == nil {
		t.Fatal("interrupted Pull returned no error")
	}

	dir, _ := c.Store.Dir("org/model")
	part := filepath.Join(dir, "model.safetensors.part")
	st, err := os.Stat(part)
	if err != nil {
		t.Fatalf("no resumable .part file left behind: %v", err)
	}
	if st.Size() == 0 || st.Size() >= int64(len(files["model.safetensors"])) {
		t.Fatalf(".part size = %d, want a partial prefix of %d", st.Size(), len(files["model.safetensors"]))
	}

	// The manifest must exist and must NOT claim readiness.
	man, err := c.Store.ReadManifest("org/model")
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if man == nil || man.Status != StatusDownloading {
		t.Fatalf("manifest after interruption = %+v, want status %q", man, StatusDownloading)
	}
	if ready, _, _ := c.Store.Ready("org/model"); ready {
		t.Fatal("a partially downloaded model reported ready")
	}

	res, err := c.Pull(context.Background(), Options{Repo: "org/model"})
	if err != nil {
		t.Fatalf("resumed Pull: %v", err)
	}
	if res.Manifest.Status != StatusReady {
		t.Fatalf("resumed Status = %q, want ready", res.Manifest.Status)
	}

	hub.mu.Lock()
	resumedAt := hub.rangeAsked["model.safetensors"]
	hub.mu.Unlock()
	if resumedAt != st.Size() {
		t.Fatalf("resumed at byte %d, want %d (the size of the .part file)", resumedAt, st.Size())
	}

	// Already-complete files are reused, not fetched again.
	joined := strings.Join(res.Reused, ",")
	if !strings.Contains(joined, "config.json") {
		t.Errorf("Reused = %v, want config.json to be reused from the interrupted run", res.Reused)
	}

	got, err := os.ReadFile(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(files["model.safetensors"]) {
		t.Fatalf("resumed file content is wrong (%d bytes, want %d)", len(got), len(files["model.safetensors"]))
	}
}

// TestPullRestartsWhenServerIgnoresRange covers the silent-corruption case: a
// server that answers a Range request with 200 and the whole body. Appending
// that onto the partial file would produce a file of the right-ish size and
// wrong content, which is exactly what the SHA256 check exists to stop -- but
// the downloader should not get there in the first place.
func TestPullRestartsWhenServerIgnoresRange(t *testing.T) {
	files := modelFiles()
	hub := newFakeHub(t, "org/model", files)
	hub.truncateAfter["model.safetensors"] = 1000
	c, _ := testClient(t, hub)

	if _, err := c.Pull(context.Background(), Options{Repo: "org/model"}); err == nil {
		t.Fatal("interrupted Pull returned no error")
	}

	hub.mu.Lock()
	hub.ignoreRange = true
	hub.mu.Unlock()

	res, err := c.Pull(context.Background(), Options{Repo: "org/model"})
	if err != nil {
		t.Fatalf("Pull against a range-ignoring server: %v", err)
	}
	if res.Manifest.Status != StatusReady {
		t.Fatalf("Status = %q, want ready", res.Manifest.Status)
	}
	dir, _ := c.Store.Dir("org/model")
	got, err := os.ReadFile(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(files["model.safetensors"]) {
		t.Fatalf("file is corrupt after a range-ignoring resume: %d bytes, want %d", len(got), len(files["model.safetensors"]))
	}
}

// TestPullRefusesDigestMismatch is the "verify every downloaded file" rule.
func TestPullRefusesDigestMismatch(t *testing.T) {
	hub := newFakeHub(t, "org/model", modelFiles())
	hub.oidOverride["model.safetensors"] = strings.Repeat("f", 64)
	c, _ := testClient(t, hub)

	_, err := c.Pull(context.Background(), Options{Repo: "org/model"})
	if err == nil {
		t.Fatal("Pull accepted a file whose sha256 did not match the published digest")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("error does not name the digest problem: %v", err)
	}

	if ready, _, _ := c.Store.Ready("org/model"); ready {
		t.Fatal("a model with a digest mismatch reported ready")
	}
	dir, _ := c.Store.Dir("org/model")
	if _, err := os.Stat(filepath.Join(dir, "model.safetensors")); err == nil {
		t.Fatal("the mismatching file was promoted out of .part")
	}
}

// TestPullRefusesRepoWithoutWeights stops a pull that would leave the Swift
// helper reporting model_not_found on a "successful" download.
func TestPullRefusesRepoWithoutWeights(t *testing.T) {
	hub := newFakeHub(t, "org/model", map[string][]byte{
		"config.json": []byte("{}"),
		"README.md":   []byte("no weights here"),
	})
	c, _ := testClient(t, hub)

	if _, err := c.Pull(context.Background(), Options{Repo: "org/model"}); err == nil {
		t.Fatal("Pull accepted a repo with no .safetensors weights")
	}
}

// TestPullRefusesTraversingFilename checks that a server-supplied name is
// never joined onto a path unchecked. This package writes files from names the
// network gave it; that is the one place a path-traversal bug would land.
func TestPullRefusesTraversingFilename(t *testing.T) {
	hub := newFakeHub(t, "org/model", modelFiles())
	hub.extraSiblings = []map[string]any{
		{"rfilename": "../../../../tmp/kagaz-escape.safetensors", "size": 4},
	}
	c, _ := testClient(t, hub)

	if _, err := c.Pull(context.Background(), Options{Repo: "org/model"}); err == nil {
		t.Fatal("Pull accepted a traversing filename")
	}
}

func TestPullRejectsBadRepoBeforeAnyRequest(t *testing.T) {
	hub := newFakeHub(t, "org/model", modelFiles())
	c, _ := testClient(t, hub)

	if _, err := c.Pull(context.Background(), Options{Repo: "../../etc"}); err == nil {
		t.Fatal("Pull accepted a traversing repo id")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.requests) != 0 {
		t.Fatalf("a rejected repo id still caused %d request(s)", len(hub.requests))
	}
}

// TestPullUsesDefaultPolicyByDefault proves the pinned host is not merely a
// default the tests overwrite: a Client with no injected policy refuses to
// fetch from an httptest server.
func TestPullUsesDefaultPolicyByDefault(t *testing.T) {
	hub := newFakeHub(t, "org/model", modelFiles())
	srv := httptest.NewServer(hub)
	defer srv.Close()

	c := &Client{Store: Store{Root: t.TempDir()}, endpoint: srv.URL}
	_, err := c.Pull(context.Background(), Options{Repo: "org/model"})
	if err == nil {
		t.Fatal("Pull with the default host policy reached a plaintext local host")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("error does not explain the https requirement: %v", err)
	}

	// And an https URL on the wrong host is refused before a connection is
	// attempted at all -- the check is on the URL, not on the dial.
	c2 := &Client{Store: Store{Root: t.TempDir()}, endpoint: "https://example.com"}
	_, err = c2.Pull(context.Background(), Options{Repo: "org/model"})
	if err == nil {
		t.Fatal("Pull with the default host policy accepted example.com")
	}
	if !strings.Contains(err.Error(), Host) {
		t.Fatalf("error does not name the pinned host: %v", err)
	}
}

func TestPullForceRedownloadsAReadyModel(t *testing.T) {
	hub := newFakeHub(t, "org/model", modelFiles())
	c, _ := testClient(t, hub)

	if _, err := c.Pull(context.Background(), Options{Repo: "org/model"}); err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	hub.mu.Lock()
	hub.requests = nil
	hub.mu.Unlock()

	res, err := c.Pull(context.Background(), Options{Repo: "org/model", Force: true})
	if err != nil {
		t.Fatalf("forced Pull: %v", err)
	}
	if res.AlreadyReady {
		t.Fatal("forced Pull short-circuited")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.requests) == 0 {
		t.Fatal("forced Pull made no requests")
	}
}
