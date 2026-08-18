package classify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
)

// remoteEndpoints are hosts that must never be dialled, whatever config says.
var remoteEndpoints = []string{
	"http://example.com:11434",
	"https://ollama.example.net",
	"http://user:pass@evil.example.com:11434",
	"http://127.0.0.1.evil.com:11434",
	"ftp://localhost:11434",
	"http://[2001:db8::1]:11434",
}

// TestOllamaRefusesNonLocalhostAtCallTime is the single most important test in
// this package: document text must never leave the machine, and the check must
// happen at call time rather than trusting config validation.
func TestOllamaRefusesNonLocalhostAtCallTime(t *testing.T) {
	for _, endpoint := range remoteEndpoints {
		t.Run(endpoint, func(t *testing.T) {
			o := &Ollama{
				Endpoint: endpoint,
				Model:    "qwen2.5:3b",
				// A transport that fails the test if it is ever used: the
				// refusal must happen before any dial.
				client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatalf("dialled %s: document text must never leave the machine", endpoint)
					return nil, nil
				})},
			}
			if o.Available() {
				t.Fatalf("Available() = true for %s", endpoint)
			}
			if _, err := o.Classify(context.Background(), Request{Text: invoiceText}); err == nil {
				t.Fatalf("Classify() accepted %s", endpoint)
			}
		})
	}
}

func TestOllamaAcceptsLoopbackHosts(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:11434",
		"http://127.0.0.1:11434",
		"http://[::1]:11434",
		"http://LOCALHOST:11434/",
	} {
		if err := requireLocalhostEndpoint(endpoint); err != nil {
			t.Errorf("requireLocalhostEndpoint(%q) = %v, want nil", endpoint, err)
		}
	}
}

// TestOllamaRejectsUnspecifiedAddress pins that 0.0.0.0 is NOT loopback.
//
// It is the address Ollama *binds* to, so it is a plausible thing for a user to
// paste into classify.endpoint -- but it is a bind address, not a destination,
// and Go's proxy bypass does not exempt it. With HTTP_PROXY or ALL_PROXY set,
// allowing it would route every document body through a proxy that need not be
// on this machine.
func TestOllamaRejectsUnspecifiedAddress(t *testing.T) {
	for _, endpoint := range []string{"http://0.0.0.0:11434", "http://[::]:11434"} {
		if err := requireLocalhostEndpoint(endpoint); err == nil {
			t.Errorf("requireLocalhostEndpoint(%q) = nil, want a refusal", endpoint)
		}
	}
}

// TestLocalOnlyClientRefusesProxyAndRedirects checks the two properties of the
// package client that the localhost check cannot provide by itself. Both are
// checked on the real client, not a test double, because a test double is
// exactly what would hide a regression here.
func TestLocalOnlyClientRefusesProxyAndRedirects(t *testing.T) {
	tr, ok := localOnlyClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("localOnlyClient.Transport is %T, want *http.Transport", localOnlyClient.Transport)
	}
	if tr.Proxy != nil {
		t.Error("localOnlyClient must not honour proxy environment variables: document text would leave the machine")
	}
	if localOnlyClient.CheckRedirect == nil {
		t.Fatal("localOnlyClient must refuse redirects")
	}
	if err := localOnlyClient.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

// TestOllamaDoesNotFollowRedirectOffHost is the exfiltration test.
//
// A local server answers 307 pointing at another host. The old code used
// http.DefaultClient, which follows up to 10 redirects and, because the body is
// a bytes.Reader with GetBody set, replays the entire prompt -- clipped
// document text and all -- to the redirect target. The chain must instead
// degrade to rules, and the off-host server must never be touched.
func TestOllamaDoesNotFollowRedirectOffHost(t *testing.T) {
	var offHostHits int32
	offHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&offHostHits, 1)
		body, _ := io.ReadAll(r.Body)
		t.Errorf("document text reached the redirect target (%d bytes): %s", len(body), firstLine(string(body)))
	}))
	defer offHost.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]string{{"name": "qwen2.5:3b", "model": "qwen2.5:3b"}},
			})
			return
		}
		http.Redirect(w, r, offHost.URL+"/api/generate", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	cat := testCatalog(t)
	// Deliberately NOT srv.Client(): the production client is what must refuse.
	o := &Ollama{Endpoint: srv.URL, Model: "qwen2.5:3b"}
	// The opt-in: this test is about the ollama tier degrading, and
	// with classify.fallback_to_rules false a forced tier that fails
	// is an error instead (see TestNamedEngineFallbackToRules).
	c := &Chain{Engine: config.EngineOllama, MinConfidence: 0.5, Catalog: cat,
		Rules: &Rules{Catalog: cat}, Ollama: o}

	got, err := c.Classify(context.Background(), Request{Text: invoiceText})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Engine != config.EngineRules || got.DocType != "invoice" {
		t.Fatalf("got %s/%s, want the rules fallback", got.DocType, got.Engine)
	}
	if n := atomic.LoadInt32(&offHostHits); n != 0 {
		t.Fatalf("the redirect target was contacted %d times, want 0", n)
	}
}

// TestOllamaRedirectErrorNamesTheRefusal checks the backend explains itself
// rather than reporting a confusing "unexpected status".
func TestOllamaRedirectErrorNamesTheRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://collector.example/api/generate", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	o := &Ollama{Endpoint: srv.URL, Model: "qwen2.5:3b"}
	_, err := o.Classify(context.Background(), Request{Text: invoiceText})
	if err == nil {
		t.Fatal("expected an error on a redirecting endpoint")
	}
	if !contains(err.Error(), "refusing to resend document text") {
		t.Fatalf("error = %q, want it to name the refusal", err)
	}
}

func TestOllamaClassify(t *testing.T) {
	answer, err := json.Marshal(ollamaAnswer{
		DocType:    "invoice",
		Category:   "travel", // the model is wrong; the catalog must win
		Confidence: 0.87,
		Fields:     map[string]string{"vendor": "Acme Corporation"},
	})
	if err != nil {
		t.Fatalf("marshalling answer: %v", err)
	}

	var gotRequest ollamaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]string{{"name": "qwen2.5:3b", "model": "qwen2.5:3b"}},
			})
		case "/api/generate":
			if err := json.NewDecoder(r.Body).Decode(&gotRequest); err != nil {
				t.Errorf("decoding request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(ollamaResponse{Response: string(answer), Done: true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cat := testCatalog(t)
	o := &Ollama{Endpoint: srv.URL, Model: "qwen2.5:3b", client: srv.Client()}
	if !o.Available() {
		t.Fatal("Available() = false against a live loopback server")
	}
	if o.Name() != config.EngineOllama {
		t.Errorf("Name() = %q, want ollama", o.Name())
	}

	// The opt-in: this test is about the ollama tier degrading, and
	// with classify.fallback_to_rules false a forced tier that fails
	// is an error instead (see TestNamedEngineFallbackToRules).
	c := &Chain{Engine: config.EngineOllama, MinConfidence: 0.5, Catalog: cat,
		Rules: &Rules{Catalog: cat}, Ollama: o}
	got, err := c.Classify(context.Background(), Request{Text: invoiceText})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got.Engine != "ollama:qwen2.5:3b" {
		t.Errorf("Engine = %q, want ollama:qwen2.5:3b", got.Engine)
	}
	if got.DocType != "invoice" || got.Category != "financial" {
		t.Errorf("got %s/%s, want invoice/financial (the catalog's category)", got.DocType, got.Category)
	}
	if gotRequest.Stream {
		t.Error("streaming must be off")
	}
	if len(gotRequest.Format) == 0 {
		t.Error("the request must carry a JSON schema so the reply is parseable")
	}
	if !contains(gotRequest.Prompt, "invoice:financial") {
		t.Error("the prompt must constrain the model to the catalog spec")
	}
}

func TestOllamaBadResponsesFallBackToRules(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"prose instead of json": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"models": []map[string]string{{"name": "qwen2.5:3b", "model": "qwen2.5:3b"}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(ollamaResponse{Response: "I think it is an invoice!", Done: true})
		},
		"http error": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"models": []map[string]string{{"name": "qwen2.5:3b", "model": "qwen2.5:3b"}},
				})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("model not found"))
		},
		"empty doctype": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"models": []map[string]string{{"name": "qwen2.5:3b", "model": "qwen2.5:3b"}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(ollamaResponse{Response: `{"doctype":"","confidence":0.9}`, Done: true})
		},
	}

	cat := testCatalog(t)
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()

			o := &Ollama{Endpoint: srv.URL, Model: "qwen2.5:3b", client: srv.Client()}
			c := &Chain{Engine: config.EngineOllama, MinConfidence: 0.5, Catalog: cat, Rules: &Rules{Catalog: cat}, Ollama: o}

			got, err := c.Classify(context.Background(), Request{Text: invoiceText})
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got.Engine != config.EngineRules || got.DocType != "invoice" {
				t.Fatalf("got %s/%s, want the rules fallback", got.DocType, got.Engine)
			}
		})
	}
}

func TestOllamaUnconfiguredModel(t *testing.T) {
	o := &Ollama{Endpoint: "http://localhost:11434"}
	if o.Available() {
		t.Error("Available() = true with no model configured")
	}
	if !contains(o.detail(), "classify.model") {
		t.Errorf("detail() = %q, want it to name the config key", o.detail())
	}
	if _, err := o.Classify(context.Background(), Request{Text: invoiceText}); err == nil {
		t.Error("Classify should fail with no model configured")
	}
	if !contains((&Ollama{Model: "m"}).detail(), "endpoint") {
		t.Error("detail() should explain a missing endpoint")
	}
	if !contains(o.hint(), "classify.model") {
		t.Errorf("hint() = %q, want it to name the fix", o.hint())
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestOllamaUnavailableWhenModelNotPulled covers the single-config-key trap:
// classify.model is shared with the MLX engine and defaults to an MLX repo
// path, so `engine: ollama` with no other setting names a model Ollama has
// never heard of. A server answering /api/tags is not enough.
func TestOllamaUnavailableWhenModelNotPulled(t *testing.T) {
	var generateHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" {
			atomic.AddInt32(&generateHits, 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "llama3.2:1b", "model": "llama3.2:1b"}},
		})
	}))
	defer srv.Close()

	o := &Ollama{Endpoint: srv.URL, Model: config.DefaultMLXModel, client: srv.Client()}
	if o.Available() {
		t.Fatal("Available() = true for a model that is not pulled")
	}
	if !contains(o.detail(), "ollama pull") {
		t.Errorf("detail() = %q, want it to name the fix", o.detail())
	}

	cat := testCatalog(t)
	// The opt-in: this test is about the ollama tier degrading, and
	// with classify.fallback_to_rules false a forced tier that fails
	// is an error instead (see TestNamedEngineFallbackToRules).
	c := &Chain{Engine: config.EngineOllama, MinConfidence: 0.5, Catalog: cat,
		Rules: &Rules{Catalog: cat}, Ollama: o}
	_, err := c.Classify(context.Background(), Request{Text: invoiceText})
	if err == nil {
		t.Fatal("forced ollama with an unpulled model should be an error, not a silent rules degradation")
	}
	if !contains(err.Error(), "ollama pull "+config.DefaultMLXModel) {
		t.Fatalf("error = %q, want it to name `ollama pull <model>`", err)
	}
	if n := atomic.LoadInt32(&generateHits); n != 0 {
		t.Errorf("/api/generate was called %d times, want 0", n)
	}
}

func TestOllamaModelListMatching(t *testing.T) {
	tags := ollamaTags{Models: []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}{
		{Name: "qwen2.5:3b", Model: "qwen2.5:3b"},
		{Name: "llama3.2:latest", Model: "llama3.2:latest"},
	}}
	for _, want := range []string{"qwen2.5:3b", "llama3.2", "llama3.2:latest", "QWEN2.5:3B"} {
		if !tags.has(want) {
			t.Errorf("has(%q) = false, want true", want)
		}
	}
	for _, want := range []string{"", "qwen2.5", "mistral", "mlx-community/Qwen2.5-3B-Instruct-4bit"} {
		if tags.has(want) {
			t.Errorf("has(%q) = true, want false", want)
		}
	}
}

// TestOllamaProbeIsCachedForTTL keeps `kagaz doctor`, which asks Available()
// and then detail(), from probing twice.
func TestOllamaProbeIsCachedForTTL(t *testing.T) {
	var probes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			atomic.AddInt32(&probes, 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "qwen2.5:3b", "model": "qwen2.5:3b"}},
		})
	}))
	defer srv.Close()

	o := &Ollama{Endpoint: srv.URL, Model: "qwen2.5:3b", client: srv.Client()}
	if !o.Available() || o.detail() == "" || !o.Available() {
		t.Fatal("expected the backend to be available")
	}
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Fatalf("probed %d times, want 1 within the TTL", n)
	}
}
