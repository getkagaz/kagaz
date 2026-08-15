package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// ollamaProbeTimeout bounds the availability check so auto-selection and
// `kagaz doctor` stay fast when nothing is listening.
const ollamaProbeTimeout = 1500 * time.Millisecond

// ollamaClassifyTimeout bounds one generation. Text models on a laptop are
// slow; this is generous but finite.
const ollamaClassifyTimeout = 2 * time.Minute

// Ollama classifies through a local Ollama server, constraining the model to a
// JSON schema so its answer is parseable rather than prose.
//
// Safety invariant: the endpoint must be loopback. That is enforced at config
// parse time *and* re-checked here before every request, so no code path can
// dial a remote host even if a Config were constructed in memory. Document text
// leaving the machine is the worst failure this codebase can have.
type Ollama struct {
	// Endpoint is the Ollama base URL, e.g. http://localhost:11434.
	Endpoint string
	// Model is the text model tag, e.g. "qwen2.5:3b".
	Model string
	// Timeout bounds one classification; zero means ollamaClassifyTimeout.
	Timeout time.Duration

	// client is a test seam; nil means http.DefaultClient.
	client *http.Client
}

// Name identifies the backend. It matches config.EngineOllama.
func (o *Ollama) Name() string { return config.EngineOllama }

// engine is the string recorded in Result.Engine.
func (o *Ollama) engine() string { return config.EngineOllama + ":" + o.Model }

// Available reports whether a localhost Ollama server is answering. It is a
// cheap GET /api/tags with a short timeout and never dials a non-loopback host.
func (o *Ollama) Available() bool {
	if o.Model == "" {
		return false
	}
	base, err := o.baseURL()
	if err != nil {
		return false
	}
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

// detail explains the backend's state for `kagaz doctor`.
func (o *Ollama) detail() string {
	if o.Model == "" {
		return "no model configured (classify.model)"
	}
	if _, err := o.baseURL(); err != nil {
		return err.Error()
	}
	if !o.Available() {
		return "no Ollama server responding at " + o.Endpoint
	}
	return o.Endpoint + " (" + o.Model + ")"
}

// hint names the fix for a forced-but-unavailable ollama engine.
func (o *Ollama) hint() string {
	return "start a local Ollama server and run `kagaz model pull --engine ollama`"
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

// ollamaSystemPrompt keeps the model inside the catalog and stops it inventing
// doctypes. Validation still assumes it will try.
const ollamaSystemPrompt = "You classify documents. Choose exactly one doctype from the provided list. " +
	"If none fits, answer with the doctype \"unclassified\" and confidence 0. " +
	"Never invent a doctype or a category. Reply with JSON only."

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
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return nil
	}
	return fmt.Errorf("ollama: endpoint %q is not localhost; Kagaz never sends document text off the machine", endpoint)
}

// httpClient returns the configured client or a sane default.
func (o *Ollama) httpClient() *http.Client {
	if o.client != nil {
		return o.client
	}
	return http.DefaultClient
}
