package ocr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getkagaz/kagaz/internal/vaultkit/config"
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
		// 0.0.0.0 is a bind address, not a destination. Go's proxy bypass
		// exempts loopback only, so with HTTP_PROXY set this would send the
		// document to the proxy host.
		{"unspecified bind address", "http://0.0.0.0:11434"},
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
		o    *Ollama
		want bool
	}{
		{"running and enabled", &Ollama{Endpoint: srv.URL, Model: "unlimited-ocr", Enabled: "true"}, true},
		{"running and auto", &Ollama{Endpoint: srv.URL, Model: "unlimited-ocr", Enabled: "auto"}, true},
		{"disabled in config", &Ollama{Endpoint: srv.URL, Model: "unlimited-ocr", Enabled: "false"}, false},
		{"no model configured", &Ollama{Endpoint: srv.URL, Enabled: "auto"}, false},
		{"no endpoint configured", &Ollama{Model: "unlimited-ocr", Enabled: "auto"}, false},
		{"server absent", &Ollama{Endpoint: deadURL, Model: "unlimited-ocr", Enabled: "auto"}, false},
		{"unspecified bind address", &Ollama{Endpoint: "http://0.0.0.0:11434", Model: "unlimited-ocr", Enabled: "auto"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.Available(); got != tc.want {
				t.Fatalf("Available() = %v, want %v (detail: %s)", got, tc.want, tc.o.detail())
			}
		})
	}
}

// TestOllamaDoctorProbesOnce pins the fix for doctor paying the probe timeout
// twice: Describe() asks Available() and then detail(), and detail() used to
// probe again.
func TestOllamaDoctorProbesOnce(t *testing.T) {
	var probes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probes, 1)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	o := &Ollama{Endpoint: srv.URL, Model: "unlimited-ocr", Enabled: "auto"}
	if !o.Available() {
		t.Fatal("Available() = false against a live stub server")
	}
	_ = o.detail()
	if got := atomic.LoadInt32(&probes); got != 1 {
		t.Fatalf("probed %d times, want 1", got)
	}
}

// TestOllamaDoesNotFollowRedirects pins the fix for a redirect escaping the
// localhost check. A local port could answer 307 with an off-host Location;
// because the request body is replayable, following it would ship the whole
// base64-encoded document to that host.
func TestOllamaDoesNotFollowRedirects(t *testing.T) {
	path, _ := writeImage(t)

	var offHostHits int32
	offHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&offHostHits, 1)
		_, _ = w.Write(readFixture(t, "ollama_trained_prompt.json"))
	}))
	defer offHost.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, offHost.URL+"/api/generate", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	o := &Ollama{Endpoint: redirector.URL, Model: "unlimited-ocr", Enabled: "true"}
	if _, err := o.Extract(context.Background(), path); err == nil {
		t.Fatal("Extract() succeeded through a redirect, want an error")
	}
	if got := atomic.LoadInt32(&offHostHits); got != 0 {
		t.Fatalf("the redirect target received %d requests, want 0: the document escaped the localhost check", got)
	}

	// The same must hold for the availability probe.
	o2 := &Ollama{Endpoint: redirector.URL, Model: "unlimited-ocr", Enabled: "auto"}
	if o2.Available() {
		t.Fatal("Available() = true for a server that only redirects")
	}
	if got := atomic.LoadInt32(&offHostHits); got != 0 {
		t.Fatalf("the redirect target received %d requests, want 0", got)
	}
}

// TestExtractorDoesNotReachOllamaWithoutOptIn is the test the config-level one
// cannot give: ocr.ollama.enabled only matters because of what the extractor
// does with it, and a vault.yaml with no `ocr:` block must leave the daemon
// untouched even when it is running with a model loaded.
//
// The two subtests share everything but the vault.yaml, so the comparison is
// exactly the default: "auto" reaches the stub daemon, an absent key does not.
// Reverting the default to "auto" makes the first subtest fail.
func TestExtractorDoesNotReachOllamaWithoutOptIn(t *testing.T) {
	// Vision must be out of the picture, or whether this machine has the
	// helper decides whether the Ollama tier is reached at all. An override
	// that does not resolve is reported unavailable, by design.
	t.Setenv(HelperPathEnv, filepath.Join(t.TempDir(), "no-such-machelper"))

	tests := []struct {
		name      string
		yaml      string
		wantHits  bool
		wantModel string
	}{
		{
			name:     "no ocr block at all",
			yaml:     "people:\n  - name: Alex Rao\n",
			wantHits: false,
		},
		{
			// The case the old default actually harmed: a model is named --
			// so nothing else stands in the way -- but nobody wrote
			// `enabled:`, and the document went to the daemon anyway.
			name:     "model named but enabled absent",
			yaml:     "people:\n  - name: Alex Rao\nocr:\n  ollama:\n    model: unlimited-ocr\n",
			wantHits: false,
		},
		{
			name:      "enabled: auto opts in",
			yaml:      "people:\n  - name: Alex Rao\nocr:\n  ollama:\n    enabled: \"auto\"\n    model: unlimited-ocr\n",
			wantHits:  true,
			wantModel: "unlimited-ocr",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				if r.URL.Path == "/api/tags" {
					_, _ = w.Write([]byte(`{"models":[{"name":"unlimited-ocr"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"response":"scanned text","done":true}`))
			}))
			defer srv.Close()

			cfg, err := config.Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			e := NewExtractor(cfg, "")
			// Only the address moves: Enabled and Model stay exactly as the
			// vault.yaml (and its defaults) produced them, which is the thing
			// under test. The stub is on loopback, so requireLocalhost is
			// satisfied and cannot be what stops the request.
			e.Ollama.Endpoint = srv.URL

			path, _ := writeImage(t)
			res, err := e.Extract(context.Background(), path)

			if got := atomic.LoadInt32(&hits) > 0; got != tc.wantHits {
				t.Fatalf("daemon contacted = %v, want %v (result %+v, err %v)", got, tc.wantHits, res, err)
			}
			if tc.wantHits {
				if err != nil {
					t.Fatalf("Extract: %v", err)
				}
				if res.Engine != "ollama:"+tc.wantModel {
					t.Errorf("Engine = %q, want %q", res.Engine, "ollama:"+tc.wantModel)
				}
			} else if !errors.Is(err, ErrNoText) {
				t.Errorf("err = %v, want ErrNoText: with no tier opted in there is nothing to extract", err)
			}
		})
	}
}

// TestOllamaDetailSeparatesNotEnabledFromNoDaemon pins what `kagaz doctor`
// reports. Since an omitted ocr.ollama.enabled now means off, "you never asked
// for this" is the common case and must not read like "your daemon is down":
// the fixes are opposite (edit vault.yaml vs start Ollama).
func TestOllamaDetailSeparatesNotEnabledFromNoDaemon(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg, err := config.Parse([]byte("people:\n  - name: Alex Rao\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	notEnabled := NewExtractor(cfg, "").Ollama.detail()
	noDaemon := (&Ollama{Endpoint: deadURL, Model: "unlimited-ocr", Enabled: config.OCROllamaAuto}).detail()

	if notEnabled == noDaemon {
		t.Fatalf("both states report %q; doctor cannot tell them apart", notEnabled)
	}
	if !strings.Contains(notEnabled, "not enabled") || !strings.Contains(notEnabled, "ocr.ollama.enabled") {
		t.Errorf("not-enabled detail = %q; it must say it is not enabled and name the key to set", notEnabled)
	}
	if !strings.Contains(noDaemon, "no Ollama server responding") {
		t.Errorf("no-daemon detail = %q; it must point at the daemon, not at the config", noDaemon)
	}
}
