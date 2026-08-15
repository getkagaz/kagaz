package ocr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusingTransport fails the test if anything tries to open a connection. It
// is how the localhost tests prove that a rejected endpoint is never dialled.
type refusingTransport struct{ t *testing.T }

func (r refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Fatalf("network access attempted to %s -- endpoint must be rejected before dialling", req.URL)
	return nil, nil
}

// writeImage creates a small fake image file and returns its path and bytes.
func writeImage(t *testing.T) (string, []byte) {
	t.Helper()
	data := []byte("\x89PNG\r\n\x1a\n fake scan bytes")
	path := filepath.Join(t.TempDir(), "scan.png")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing image: %v", err)
	}
	return path, data
}

func TestOllamaRejectsNonLocalhostWithoutDialling(t *testing.T) {
	path, _ := writeImage(t)
	tests := []struct {
		name     string
		endpoint string
	}{
		{"remote host", "http://evil.example.com"},
		{"remote host with port", "http://evil.example.com:11434"},
		{"remote ip", "http://203.0.113.7:11434"},
		{"credentials disguising the host", "http://localhost@evil.example.com:11434"},
		{"https remote", "https://ollama.example.com/api"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := &Ollama{
				Endpoint: tc.endpoint,
				Model:    "llama3.2-vision",
				Enabled:  "true",
				client:   &http.Client{Transport: refusingTransport{t}},
			}

			_, err := o.Extract(context.Background(), path)
			if err == nil {
				t.Fatal("Extract() succeeded against a non-localhost endpoint")
			}
			if !strings.Contains(err.Error(), "not localhost") {
				t.Fatalf("error = %q, want a localhost rejection", err)
			}
			if o.Available() {
				t.Fatal("Available() = true for a non-localhost endpoint")
			}
		})
	}
}

func TestOllamaAcceptsLoopbackEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:11434",
		"http://127.0.0.1:11434",
		"http://[::1]:11434",
		"http://0.0.0.0:11434",
		"http://LocalHost:11434/",
	} {
		if err := requireLocalhostEndpoint(endpoint); err != nil {
			t.Fatalf("requireLocalhostEndpoint(%q) = %v, want nil", endpoint, err)
		}
	}
}

func TestOllamaExtract(t *testing.T) {
	path, imageData := writeImage(t)

	var got ollamaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("path = %q, want /api/generate", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "ollama_trained_prompt.json"))
	}))
	defer srv.Close()

	o := &Ollama{Endpoint: srv.URL, Model: "unlimited-ocr", Enabled: "true"}
	res, err := o.Extract(context.Background(), path)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}

	if got.Stream {
		t.Error("request set stream:true, want stream:false")
	}
	if len(got.Images) != 1 || got.Images[0] != base64.StdEncoding.EncodeToString(imageData) {
		t.Errorf("images = %v, want the base64-encoded file", got.Images)
	}
	if res.Engine != "ollama:unlimited-ocr" {
		t.Errorf("Engine = %q, want %q", res.Engine, "ollama:unlimited-ocr")
	}
	if !strings.Contains(res.Text, "Invoice 2024-117") {
		t.Errorf("Text = %q, want the transcription", res.Text)
	}
	if res.Pages != 1 {
		t.Errorf("Pages = %d, want 1", res.Pages)
	}
}

// TestOllamaPromptPerModel is the regression test for the recorded quirk that
// unlimited-ocr returns an EMPTY response for any prompt but its trained
// phrase. The stub server replays the recorded empty-response fixture whenever
// the wrong prompt arrives, so a change to the prompt map fails loudly here
// instead of silently producing blank OCR in production.
func TestOllamaPromptPerModel(t *testing.T) {
	path, _ := writeImage(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if normalizeModelName(req.Model) == "unlimited-ocr" && req.Prompt != ollamaPrompts["unlimited-ocr"] {
			_, _ = w.Write(readFixture(t, "ollama_empty_response.json"))
			return
		}
		_, _ = w.Write(readFixture(t, "ollama_trained_prompt.json"))
	}))
	defer srv.Close()

	tests := []struct {
		name       string
		model      string
		wantPrompt string
		wantText   bool
	}{
		{"quirky model gets its trained phrase", "unlimited-ocr", ollamaPrompts["unlimited-ocr"], true},
		{"tag is ignored", "unlimited-ocr:q4_K_M", ollamaPrompts["unlimited-ocr"], true},
		{"registry prefix is ignored", "hf.co/acme/unlimited-ocr:latest", ollamaPrompts["unlimited-ocr"], true},
		{"case is ignored", "Unlimited-OCR", ollamaPrompts["unlimited-ocr"], true},
		{"ordinary model gets the default prompt", "llama3.2-vision:11b", DefaultOllamaPrompt, true},
		{"unknown model gets the default prompt", "some-new-vision-model", DefaultOllamaPrompt, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptForModel(tc.model); got != tc.wantPrompt {
				t.Fatalf("PromptForModel(%q) = %q, want %q", tc.model, got, tc.wantPrompt)
			}
			o := &Ollama{Endpoint: srv.URL, Model: tc.model, Enabled: "true"}
			res, err := o.Extract(context.Background(), path)
			if err != nil {
				t.Fatalf("Extract() error = %v", err)
			}
			if tc.wantText && strings.TrimSpace(res.Text) == "" {
				t.Fatal("Extract() returned empty text: the wrong prompt was sent")
			}
		})
	}
}

func TestOllamaExtractServerError(t *testing.T) {
	path, _ := writeImage(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	o := &Ollama{Endpoint: srv.URL, Model: "missing-model", Enabled: "true"}
	if _, err := o.Extract(context.Background(), path); err == nil {
		t.Fatal("Extract() succeeded on a 404")
	} else if !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("error = %q, want the server message", err)
	}
}

func TestOllamaAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("path = %q, want /api/tags", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"unlimited-ocr"}]}`))
	}))
	defer srv.Close()

	// A server that is not listening: httptest hands back a loopback URL that
	// now refuses connections.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	tests := []struct {
		name string
		o    Ollama
		want bool
	}{
		{"running and enabled", Ollama{Endpoint: srv.URL, Model: "unlimited-ocr", Enabled: "true"}, true},
		{"running and auto", Ollama{Endpoint: srv.URL, Model: "unlimited-ocr", Enabled: "auto"}, true},
		{"disabled in config", Ollama{Endpoint: srv.URL, Model: "unlimited-ocr", Enabled: "false"}, false},
		{"no model configured", Ollama{Endpoint: srv.URL, Enabled: "auto"}, false},
		{"no endpoint configured", Ollama{Model: "unlimited-ocr", Enabled: "auto"}, false},
		{"server absent", Ollama{Endpoint: deadURL, Model: "unlimited-ocr", Enabled: "auto"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.o
			if got := o.Available(); got != tc.want {
				t.Fatalf("Available() = %v, want %v (detail: %s)", got, tc.want, o.detail())
			}
		})
	}
}
