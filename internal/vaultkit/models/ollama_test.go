package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireLocalhost(t *testing.T) {
	tests := []struct {
		endpoint string
		ok       bool
	}{
		{"http://localhost:11434", true},
		{"http://127.0.0.1:11434", true},
		{"http://[::1]:11434", true},
		{"http://localhost:11434/", true},
		// 0.0.0.0 is a bind address, not a destination, and Go's proxy bypass
		// does not treat it as loopback -- the exact bug found in a sibling
		// package.
		{"http://0.0.0.0:11434", false},
		{"http://ollama.example.com:11434", false},
		{"https://example.com", false},
		{"ftp://localhost", false},
		{"", false},
		{"localhost:11434", false}, // no scheme: url.Parse reads it as a scheme
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			_, err := RequireLocalhost(tt.endpoint)
			if tt.ok != (err == nil) {
				t.Fatalf("RequireLocalhost(%q) = %v, want ok=%v", tt.endpoint, err, tt.ok)
			}
		})
	}
}

func TestOllamaPullRefusesNonLocalhostBeforeDialing(t *testing.T) {
	var dialed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed = true
	}))
	defer srv.Close()

	p := &OllamaPuller{Endpoint: "http://ollama.example.com:11434", client: srv.Client()}
	err := p.Pull(context.Background(), "llama3.2", nil)
	if err == nil {
		t.Fatal("Pull accepted a non-localhost endpoint")
	}
	if !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("error does not explain the localhost rule: %v", err)
	}
	if dialed {
		t.Fatal("a rejected endpoint still caused a request")
	}
}

func TestOllamaPullDelegatesAndStreamsStatus(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model

		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, s := range []string{"pulling manifest", "downloading", "downloading", "verifying sha256", "success"} {
			_, _ = w.Write([]byte(`{"status":"` + s + `"}` + "\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	var seen []string
	p := &OllamaPuller{Endpoint: srv.URL, client: srv.Client()}
	if err := p.Pull(context.Background(), "llama3.2", func(s string) { seen = append(seen, s) }); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if gotModel != "llama3.2" {
		t.Fatalf("daemon got model %q, want llama3.2", gotModel)
	}
	// Repeated identical statuses are collapsed, so a progress line does not
	// scroll uselessly.
	want := []string{"pulling manifest", "downloading", "verifying sha256", "success"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("progress = %v, want %v", seen, want)
	}
}

func TestOllamaPullSurfacesDaemonError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"model \"nope\" not found"}` + "\n"))
	}))
	defer srv.Close()

	p := &OllamaPuller{Endpoint: srv.URL, client: srv.Client()}
	err := p.Pull(context.Background(), "nope", nil)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Pull error = %v, want the daemon's message", err)
	}
}

func TestOllamaPullRejectsEmptyModel(t *testing.T) {
	p := &OllamaPuller{Endpoint: "http://localhost:11434"}
	if err := p.Pull(context.Background(), "  ", nil); err == nil {
		t.Fatal("Pull accepted an empty model name")
	}
}
