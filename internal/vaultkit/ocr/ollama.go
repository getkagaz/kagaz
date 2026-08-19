package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// DefaultOllamaPrompt is used for any model without an entry in ollamaPrompts.
const DefaultOllamaPrompt = "Transcribe all of the text in this image exactly as it appears, preserving line breaks and reading order. Output only the text."

// ollamaPrompts maps a model to the exact prompt phrase it was trained on.
//
// Some OCR models are fine-tuned on one instruction and treat anything else as
// noise. The recorded case is `unlimited-ocr`, whose GGUF builds return an
// EMPTY response for freeform prompts and only produce text for their trained
// phrase -- a silent failure that looks like "the model can't read this scan".
//
// Keys are the bare model name: any registry prefix (`hf.co/user/`) and any tag
// (`:q4_K_M`) are stripped before lookup, and matching is case-insensitive.
// Teaching Kagaz about another quirky model is one line here.
var ollamaPrompts = map[string]string{
	// TODO(spec): this phrase is UNVERIFIED. docs/model-use.md (§6 of the
	// spec) is the authority for unlimited-ocr's exact trained prompt; the
	// value below is a reconstruction. Confirm it against the model card /
	// spec and correct it here -- an incorrect phrase makes the model return
	// empty output, which looks like a bad scan rather than a wrong prompt.
	"unlimited-ocr": "Read all the text in the image.",
}

// Ollama runs a local vision model through Ollama's HTTP API. It is opt-in: it
// is slower than Vision and needs model weights on disk, but it handles layouts
// Vision struggles with.
//
// Safety invariant: the endpoint must be localhost. That is enforced at config
// parse time *and* re-checked here on every call, so no code path can dial a
// remote host even if a Config were constructed in memory.
type Ollama struct {
	// Endpoint is the Ollama base URL, e.g. http://localhost:11434.
	Endpoint string
	// Model is the vision model tag, e.g. "llama3.2-vision:11b".
	Model string
	// Enabled is a tri-state: config.OCROllamaAuto, OCROllamaOn or
	// OCROllamaOff. Only the first two are an opt-in; see optedIn.
	Enabled string

	// client is a test seam; nil means localOnlyClient is used.
	client *http.Client

	// probeMu guards the cached Available() result. Ollama therefore must not
	// be copied after first use; ocr.go holds it by pointer.
	probeMu  sync.Mutex
	probedAt time.Time
	probeOK  bool
}

// localOnlyClient is the package's HTTP client for the Ollama runner. Two of
// its settings are load-bearing safety properties, not tuning:
//
//   - CheckRedirect refuses to follow redirects. Without it, anything answering
//     on 127.0.0.1 could reply "307 Location: https://example.com/" and Go
//     would replay the request body -- the whole base64-encoded document -- to
//     a remote host, entirely bypassing the localhost check.
//   - Proxy is nil. Go's default proxy bypass exempts loopback addresses only,
//     so with HTTP_PROXY set a non-loopback local endpoint would route document
//     bytes through a proxy that may not be on this machine.
var localOnlyClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 0,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	},
}

// ollamaProbeTimeout bounds the availability check so `kagaz doctor` and the
// auto-selection path stay fast when nothing is listening.
const ollamaProbeTimeout = 1500 * time.Millisecond

// ollamaProbeTTL is how long an availability answer is reused. It exists so a
// single `kagaz doctor` run, which asks Available() and then detail(), probes
// once rather than twice, while still noticing a server that starts later.
const ollamaProbeTTL = 5 * time.Second

// ollamaExtractTimeout bounds a generation. Vision models on a laptop are slow;
// this is generous but finite.
const ollamaExtractTimeout = 5 * time.Minute

// Name identifies the runner in doctor output. Result.Engine additionally
// carries the model name.
func (o *Ollama) Name() string { return "ollama" }

// Available reports whether Ollama is opted in, configured with a localhost
// endpoint and answering. It is a cheap GET /api/tags with a short timeout and
// never dials a non-localhost host.
//
// The result is cached for ollamaProbeTTL so that `kagaz doctor`, which asks
// for availability and then for detail, does not pay the timeout twice.
func (o *Ollama) Available() bool {
	if !o.optedIn() {
		return false
	}
	if o.Model == "" {
		return false
	}
	base, err := o.baseURL()
	if err != nil {
		return false
	}

	o.probeMu.Lock()
	defer o.probeMu.Unlock()
	if !o.probedAt.IsZero() && time.Since(o.probedAt) < ollamaProbeTTL {
		return o.probeOK
	}
	o.probeOK = o.probe(base)
	o.probedAt = time.Now()
	return o.probeOK
}

// optedIn reports whether the vault asked for this runner. Only the two
// affirmative values count, so the zero value -- an Ollama built from a Config
// that never went through config.Defaults -- reads as "not asked for" rather
// than as "auto". Sending a document to a model is the expensive, irreversible
// direction, so the only way to be wrong here is closed.
func (o *Ollama) optedIn() bool {
	return o.Enabled == config.OCROllamaAuto || o.Enabled == config.OCROllamaOn
}

// probe performs the actual GET /api/tags. The caller holds probeMu.
func (o *Ollama) probe(base string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), ollamaProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode == http.StatusOK
}

// detail explains the runner's state for `kagaz doctor`.
func (o *Ollama) detail() string {
	// "Not enabled" and "enabled but nothing is listening" are two different
	// situations with two different fixes, and since ocr.ollama.enabled now
	// defaults to off, the first is what most vaults see. They must not read
	// as the same sentence: one asks the user to opt in, the other to start a
	// daemon.
	if !o.optedIn() {
		return "not enabled: ocr.ollama.enabled is " + config.OCROllamaOff +
			", which is also the default; set it to " + config.OCROllamaAuto +
			" or " + config.OCROllamaOn + " to send documents to a local vision model"
	}
	if o.Model == "" {
		return "no model configured (ocr.ollama.model)"
	}
	if _, err := o.baseURL(); err != nil {
		return err.Error()
	}
	if !o.Available() {
		return "no Ollama server responding at " + o.Endpoint
	}
	return o.Endpoint + " (" + o.Model + ")"
}

// Extract sends the document to the local vision model and returns its
// transcription. The endpoint is re-validated here, before any network use, so
// a non-localhost endpoint fails without a dial ever happening.
func (o *Ollama) Extract(ctx context.Context, path string) (Result, error) {
	base, err := o.baseURL()
	if err != nil {
		return Result{Engine: "none"}, err
	}
	if o.Model == "" {
		return Result{Engine: "none"}, fmt.Errorf("ollama: no model configured (ocr.ollama.model)")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("ollama: reading %s: %w", path, err)
	}

	body, err := json.Marshal(ollamaRequest{
		Model:  o.Model,
		Prompt: PromptForModel(o.Model),
		Stream: false,
		Images: []string{base64.StdEncoding.EncodeToString(data)},
	})
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("ollama: encoding request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, ollamaExtractTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("ollama: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient().Do(req)
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Result{Engine: "none"}, fmt.Errorf("ollama: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{Engine: "none"}, fmt.Errorf("ollama: %s: %s", resp.Status, firstLine(strings.TrimSpace(string(payload))))
	}

	var out ollamaResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return Result{Engine: "none"}, fmt.Errorf("ollama: decoding response: %w", err)
	}

	text := strings.TrimSpace(out.Response)
	res := Result{
		Text:   text,
		Engine: "ollama:" + o.Model,
		// Ollama reports no per-token confidence; leave it unset rather than
		// inventing a number the caller might threshold on.
		Confidence: 0,
		Pages:      1,
	}
	if text == "" {
		return res, ErrNoText
	}
	return res, nil
}

// ollamaRequest is the /api/generate payload.
type ollamaRequest struct {
	Model  string   `json:"model"`
	Prompt string   `json:"prompt"`
	Stream bool     `json:"stream"`
	Images []string `json:"images,omitempty"`
}

// ollamaResponse is the non-streaming /api/generate reply.
type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// PromptForModel returns the prompt phrase to send to a model, honouring the
// per-model overrides for models that only respond to their trained phrase.
// Registry prefixes and tags are ignored, so "hf.co/acme/unlimited-ocr:q4_K_M"
// resolves the same entry as "unlimited-ocr".
func PromptForModel(model string) string {
	if p, ok := ollamaPrompts[normalizeModelName(model)]; ok {
		return p
	}
	return DefaultOllamaPrompt
}

// normalizeModelName strips registry prefix and tag and lowercases the result.
func normalizeModelName(model string) string {
	name := strings.TrimSpace(model)
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(name)
}

// baseURL validates the endpoint and returns it without a trailing slash. It
// performs no I/O, so a rejected endpoint is rejected before any dial.
func (o *Ollama) baseURL() (string, error) {
	endpoint := strings.TrimSpace(o.Endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("ollama: no endpoint configured")
	}
	if err := requireLocalhostEndpoint(endpoint); err != nil {
		return "", err
	}
	return strings.TrimSuffix(endpoint, "/"), nil
}

// requireLocalhostEndpoint enforces the no-network invariant: Kagaz never sends
// document contents off the machine, so only loopback hosts are dialable. This
// is re-checked at call time and never trusts config validation alone.
func requireLocalhostEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("ollama: invalid endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("ollama: endpoint %q must be http or https", endpoint)
	}
	// A literal allowlist, deliberately: resolving a name would let DNS decide
	// what counts as local. 0.0.0.0 is NOT allowed -- it is a bind address, not
	// a destination, and Go's proxy bypass does not treat it as loopback.
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	return fmt.Errorf("ollama: endpoint %q is not localhost; Kagaz never sends document text off the machine", endpoint)
}

// httpClient returns the test-injected client, or the package's redirect- and
// proxy-refusing client. It never falls back to http.DefaultClient, which would
// follow redirects and honour proxy environment variables.
func (o *Ollama) httpClient() *http.Client {
	if o.client != nil {
		return o.client
	}
	return localOnlyClient
}
