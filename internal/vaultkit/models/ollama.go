package models

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ollamaPullTimeout bounds a delegated pull. Model downloads are large and a
// user watching a progress line will wait; an unbounded call will not.
const ollamaPullTimeout = 2 * time.Hour

// localOnlyClient is the client for the Ollama daemon. It is the same
// hardened shape as the one in internal/vaultkit/ocr, and for the same two
// reasons:
//
//   - CheckRedirect refuses every redirect. Anything answering on 127.0.0.1
//     could otherwise reply "307 Location: https://example.com/" and Go would
//     replay the request to a remote host, straight through the localhost check.
//   - Proxy is nil. Go's default proxy bypass exempts loopback only, so with
//     HTTP_PROXY set, a non-loopback "local" endpoint would route through a
//     proxy that may not be on this machine.
var localOnlyClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        2,
		IdleConnTimeout:     30 * time.Second,
	},
}

// OllamaPuller delegates a model pull to a local Ollama daemon.
//
// Kagaz never fetches Ollama weights itself: it asks the daemon already
// running on this machine to do it. Whatever network activity follows is
// Ollama's, on the user's own configured terms. From Kagaz's side this is a
// localhost call like every other Ollama call, and the endpoint is
// re-validated here at call time rather than trusted from config.
type OllamaPuller struct {
	// Endpoint is the daemon's base URL, e.g. http://localhost:11434.
	Endpoint string

	// Log receives the informational license note and any other human-readable
	// lines. Nil falls back to the progress callback, and then to discarding.
	Log func(string)

	// client is a test seam; nil means localOnlyClient.
	client *http.Client
}

// Pull asks the local daemon to pull model, streaming its status lines to
// progress (which may be nil).
func (p *OllamaPuller) Pull(ctx context.Context, model string, progress func(string)) error {
	logf := p.Log
	if logf == nil {
		logf = progress
	}
	if logf == nil {
		logf = func(string) {}
	}

	base, err := RequireLocalhost(p.Endpoint)
	if err != nil {
		return err
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("models: no ollama model named")
	}

	// The same informational, non-gating note the MLX path prints. Kagaz is not
	// fetching these weights -- the local daemon is -- but the user is still
	// acquiring a third-party model on Kagaz's prompting, and is still the one
	// responsible for its licence.
	logf(OllamaLicenseNote(model))

	body, err := json.Marshal(map[string]any{"model": model, "stream": true})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, ollamaPullTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := p.client
	if client == nil {
		client = localOnlyClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("models: ollama: %w (is `ollama serve` running?)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("models: ollama: %s: %s", resp.Status, firstLine(strings.TrimSpace(string(payload))))
	}

	// The daemon streams one JSON object per line until it is done.
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 64<<20))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var last string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Error != "" {
			return fmt.Errorf("models: ollama: %s", msg.Error)
		}
		if msg.Status != "" && msg.Status != last {
			last = msg.Status
			if progress != nil {
				progress(msg.Status)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("models: ollama: reading response: %w", err)
	}
	if last == "" {
		return fmt.Errorf("models: ollama: the daemon reported no status; the pull may not have started")
	}
	return nil
}

// RequireLocalhost enforces the no-network invariant for the Ollama path and
// returns the endpoint without a trailing slash. It performs no I/O, so a
// rejected endpoint is rejected before any dial happens.
//
// The allowlist is literal on purpose: resolving a name would let DNS decide
// what counts as local. 0.0.0.0 is NOT allowed -- it is a bind address, not a
// destination, and Go's proxy bypass does not treat it as loopback.
func RequireLocalhost(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("models: no ollama endpoint configured")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("models: invalid ollama endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("models: ollama endpoint %q must be http or https", endpoint)
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return strings.TrimSuffix(endpoint, "/"), nil
	}
	return "", fmt.Errorf("models: ollama endpoint %q is not localhost; Kagaz delegates ollama pulls only to a daemon on this machine", endpoint)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
