package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
	"github.com/getkagaz/kagaz/internal/vaultkit/doctypes"
)

// localOnlyClient is the package's HTTP client for the Ollama classify backend.
// Two of its settings are load-bearing safety properties, not tuning, and both
// close a hole that the localhost check alone does not:
//
//   - CheckRedirect refuses to follow redirects. Without it, anything answering
//     on 127.0.0.1:11434 -- a dev proxy, a hijacked port, a malicious local app
//     -- could reply "307 Location: https://collector.example/" and Go would
//     replay the request body, which is the document's text plus the catalog,
//     to a remote host. The localhost check runs once on the endpoint and never
//     again per hop, so redirects bypass it entirely.
//   - Proxy is nil. Go's ProxyFromEnvironment bypass exempts genuine loopback
//     only, so with HTTP_PROXY/ALL_PROXY set, a non-loopback local-looking
//     endpoint would route every document body through a proxy that may not be
//     on this machine.
//
// http.DefaultClient must never be used here: it does both of those things.
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

// ollamaProbeTimeout bounds the availability check so auto-selection and
// `kagaz doctor` stay fast when nothing is listening.
const ollamaProbeTimeout = 1500 * time.Millisecond

// ollamaProbeTTL is how long an availability answer is reused, so a single
// `kagaz doctor` run -- which asks Available() and then detail() -- probes once
// rather than twice, while still noticing a server that starts later.
const ollamaProbeTTL = 5 * time.Second

// ollamaClassifyTimeout bounds one generation. Text models on a laptop are
// slow; this is generous but finite.
const ollamaClassifyTimeout = 2 * time.Minute

// Ollama classifies through a local Ollama server, constraining the model to a
// JSON schema so its answer is parseable rather than prose.
//
// Safety invariant: the endpoint must be loopback, and the transport must
// neither follow redirects nor honour proxy environment variables. That is
// enforced at config parse time *and* re-checked here before every request, so
// no code path can send document text to a remote host even if a Config were
// constructed in memory. Document text leaving the machine is the worst failure
// this codebase can have.
//
// Ollama must not be copied after first use: it caches its probe behind a
// mutex. Hold it by pointer, as Chain does.
type Ollama struct {
	// Endpoint is the Ollama base URL, e.g. http://localhost:11434.
	Endpoint string
	// Model is the text model tag, e.g. "qwen2.5:3b".
	Model string
	// Timeout bounds one classification; zero means ollamaClassifyTimeout.
	Timeout time.Duration

	// client is a test seam; nil means localOnlyClient.
	client *http.Client

	// probeMu guards the cached Available() answer.
	probeMu   sync.Mutex
	probedAt  time.Time
	probeOK   bool
	probeWhy  string
	probeCode string
}

// Name identifies the backend. It matches config.EngineOllama.
func (o *Ollama) Name() string { return config.EngineOllama }

// engine is the string recorded in Result.Engine.
func (o *Ollama) engine() string { return config.EngineOllama + ":" + o.Model }

// Available reports whether a localhost Ollama server is answering *and* has
// the configured model pulled.
//
// The model check is not pedantry. classify.model has no default -- naming a
// model the user never chose would be a lie in every sidecar's provenance --
// so `engine: ollama` with nothing else set is the common case, and a stale
// vault may still carry a name this daemon has never pulled. Without this
// check the forced-engine guard passes, every /api/generate 404s, and every
// document silently degrades to rules with no error and no hint.
func (o *Ollama) Available() bool {
	ok, _, _ := o.availability()
	return ok
}

// availability returns the cached probe result, the prose reason when
// unavailable, and the machine-readable code for it.
func (o *Ollama) availability() (bool, string, string) {
	if o.Model == "" {
		return false, "no model configured (classify.model)", ReasonModelNotConfigured
	}
	base, err := o.baseURL()
	if err != nil {
		return false, err.Error(), ReasonDaemonUnreachable
	}

	o.probeMu.Lock()
	defer o.probeMu.Unlock()
	if !o.probedAt.IsZero() && time.Since(o.probedAt) < ollamaProbeTTL {
		return o.probeOK, o.probeWhy, o.probeCode
	}
	o.probeOK, o.probeWhy, o.probeCode = o.probe(base)
	o.probedAt = time.Now()
	return o.probeOK, o.probeWhy, o.probeCode
}

// reason names WHICH precondition is unmet, from the vocabulary in helper.go.
// Ollama's weights are the daemon's, never Kagaz's, so it never reports
// ReasonWeightsMissing: `ollama pull` is the fix, not `kagaz model pull`.
func (o *Ollama) reason() string {
	ok, _, code := o.availability()
	if ok {
		return ""
	}
	return code
}

// probe performs GET /api/tags and checks the configured model is listed. The
// caller holds probeMu.
func (o *Ollama) probe(base string) (bool, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), ollamaProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return false, err.Error(), ReasonDaemonUnreachable
	}
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return false, "no Ollama server responding at " + o.Endpoint, ReasonDaemonUnreachable
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, "no Ollama server responding at " + o.Endpoint, ReasonDaemonUnreachable
	}
	if resp.StatusCode != http.StatusOK {
		return false, "Ollama at " + o.Endpoint + " answered " + resp.Status, ReasonDaemonUnreachable
	}

	var tags ollamaTags
	if err := json.Unmarshal(payload, &tags); err != nil {
		return false, "Ollama at " + o.Endpoint + " returned an unreadable model list", ReasonDaemonUnreachable
	}
	if !tags.has(o.Model) {
		return false, "model " + o.Model + " is not pulled; run `ollama pull " + o.Model + "`", ReasonModelNotPulled
	}
	return true, "", ""
}

// detail explains the backend's state for `kagaz doctor`.
func (o *Ollama) detail() string {
	ok, why, _ := o.availability()
	if ok {
		return o.Endpoint + " (" + o.Model + ")"
	}
	return why
}

// hint names the fix for a forced-but-unavailable ollama engine. It names the
// model, because "not available" most often means "that model is not pulled".
func (o *Ollama) hint() string {
	if o.Model == "" {
		return "set classify.model and start a local Ollama server"
	}
	return "start a local Ollama server and run `ollama pull " + o.Model + "`"
}

// ollamaTags is the GET /api/tags reply.
type ollamaTags struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

// has reports whether want is among the pulled models. Ollama reports tagged
// names ("qwen2.5:3b"), and an untagged request means the ":latest" tag, so
// both spellings are accepted.
func (t ollamaTags) has(want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	candidates := []string{want}
	if !strings.Contains(want, ":") {
		candidates = append(candidates, want+":latest")
	}
	for _, m := range t.Models {
		for _, c := range candidates {
			if strings.EqualFold(m.Name, c) || strings.EqualFold(m.Model, c) {
				return true
			}
		}
	}
	return false
}

// Classify asks the local model for a doctype, constrained to a JSON schema.
// The endpoint is re-validated before any network use, so a non-loopback
// endpoint fails without a dial ever happening.
func (o *Ollama) Classify(ctx context.Context, req Request) (Result, error) {
	base, err := o.baseURL()
	if err != nil {
		return Result{}, err
	}
	if o.Model == "" {
		return Result{}, fmt.Errorf("ollama: no model configured (classify.model)")
	}

	body, err := json.Marshal(ollamaRequest{
		Model:  o.Model,
		System: ollamaSystemPrompt,
		Prompt: ollamaPrompt(req),
		Stream: false,
		Format: json.RawMessage(ollamaSchema),
		Options: map[string]any{
			// Deterministic output: the same document must classify the same
			// way twice, or a re-ingest silently disagrees with the sidecar.
			"temperature": 0,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("ollama: encoding request: %w", err)
	}

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = ollamaClassifyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("ollama: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient().Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Result{}, fmt.Errorf("ollama: reading response: %w", err)
	}
	// A redirect reaches here as a 3xx rather than being followed, because
	// localOnlyClient returns ErrUseLastResponse. Treat it as the refusal it
	// is, and say so: a redirecting "local" Ollama is an exfiltration attempt.
	if isRedirect(resp.StatusCode) {
		return Result{}, fmt.Errorf("ollama: %s redirected to %q; refusing to resend document text to another host",
			o.Endpoint, resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("ollama: %s: %s", resp.Status, firstLine(strings.TrimSpace(string(payload))))
	}

	var out ollamaResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return Result{}, fmt.Errorf("ollama: decoding response: %w", err)
	}

	var answer ollamaAnswer
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.Response)), &answer); err != nil {
		return Result{}, fmt.Errorf("ollama: model did not return the requested JSON: %w", err)
	}
	if strings.TrimSpace(answer.DocType) == "" {
		return Result{}, fmt.Errorf("ollama: model returned no doctype")
	}

	return Result{
		DocType:    answer.DocType,
		Category:   answer.Category, // recorded but overwritten by the catalog
		Confidence: answer.Confidence,
		Fields:     answer.Fields,
		Engine:     o.engine(),
	}, nil
}

// isRedirect reports whether a status code is a redirect.
func isRedirect(code int) bool { return code >= 300 && code <= 399 }

// ollamaSystemPrompt keeps the model inside the catalog and stops it inventing
// doctypes. Validation still assumes it will try.
const ollamaSystemPrompt = "You classify documents. Choose exactly one doctype from the provided list. " +
	"If none of them genuinely fits, answer with the doctype \"unclassified\" and confidence 0; " +
	"prefer \"unclassified\" over a near miss, because a near miss files the document in the wrong place. " +
	"Otherwise confidence is your certainty from 0.0 to 1.0: reserve 0.8 and above for a document that " +
	"plainly announces its own type. Never invent a doctype or a category. Reply with JSON only."

// ollamaSchema constrains the reply. Ollama's `format` accepts a JSON schema
// and enforces it during sampling, which is what makes this parseable without
// prose-stripping heuristics.
const ollamaSchema = `{
  "type": "object",
  "properties": {
    "doctype": {"type": "string"},
    "category": {"type": "string"},
    "confidence": {"type": "number"},
    "fields": {"type": "object", "additionalProperties": {"type": "string"}}
  },
  "required": ["doctype", "confidence"]
}`

// ollamaPrompt renders the user turn: the allowed doctypes and the text.
func ollamaPrompt(req Request) string {
	var b strings.Builder
	if spec := req.spec(); spec != "" {
		b.WriteString("Allowed doctypes as \"name:category\" pairs:\n")
		b.WriteString(spec)
		// The escape hatch is listed with the rest so it reads as one of the
		// choices rather than as a caveat. It carries no category: the Go core
		// supplies none for an unclassified document.
		b.WriteString(",")
		b.WriteString(doctypes.Unclassified)
		b.WriteString("\n\n")
	}
	b.WriteString("Document text:\n")
	b.WriteString(req.text())
	return b.String()
}

// ollamaRequest is the /api/generate payload.
type ollamaRequest struct {
	Model   string          `json:"model"`
	System  string          `json:"system,omitempty"`
	Prompt  string          `json:"prompt"`
	Stream  bool            `json:"stream"`
	Format  json.RawMessage `json:"format,omitempty"`
	Options map[string]any  `json:"options,omitempty"`
}

// ollamaResponse is the non-streaming /api/generate reply.
type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// ollamaAnswer is the schema-constrained JSON inside that reply.
type ollamaAnswer struct {
	DocType    string            `json:"doctype"`
	Category   string            `json:"category"`
	Confidence float64           `json:"confidence"`
	Fields     map[string]string `json:"fields"`
}

// baseURL validates the endpoint and returns it without a trailing slash. It
// performs no I/O, so a rejected endpoint is rejected before any dial.
func (o *Ollama) baseURL() (string, error) {
	endpoint := strings.TrimSpace(o.Endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("ollama: no endpoint configured (classify.endpoint)")
	}
	if err := requireLocalhostEndpoint(endpoint); err != nil {
		return "", err
	}
	return strings.TrimSuffix(endpoint, "/"), nil
}

// requireLocalhostEndpoint enforces the no-network invariant. It is re-checked
// at call time and never trusts config validation alone, because a Config can
// be built in memory without ever passing through config.Validate.
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
	// a destination, and Go's proxy bypass does not treat it as loopback, so a
	// machine with HTTP_PROXY set would route document text through the proxy.
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
