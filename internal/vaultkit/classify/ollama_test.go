package classify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"http://0.0.0.0:11434",
		"http://LOCALHOST:11434/",
	} {
		if err := requireLocalhostEndpoint(endpoint); err != nil {
			t.Errorf("requireLocalhostEndpoint(%q) = %v, want nil", endpoint, err)
		}
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
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
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

	c := &Chain{Engine: config.EngineOllama, MinConfidence: 0.5, Catalog: cat, Rules: &Rules{Catalog: cat}, Ollama: o}
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
				return
			}
			_ = json.NewEncoder(w).Encode(ollamaResponse{Response: "I think it is an invoice!", Done: true})
		},
		"http error": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("model not found"))
		},
		"empty doctype": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/tags" {
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
	if !contains(o.hint(), "kagaz model pull") {
		t.Errorf("hint() = %q, want it to name the fix", o.hint())
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
