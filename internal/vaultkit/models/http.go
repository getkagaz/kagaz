package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// userAgent identifies Kagaz to the hub. It carries no user, vault or document
// information -- the only thing this package ever discloses is a public model
// id.
const userAgent = "kagaz-model-pull/1"

// errRangeNotSatisfiable signals HTTP 416: the resume offset is at or past the
// end of the file, which means the local `.part` is already complete.
var errRangeNotSatisfiable = errors.New("requested range not satisfiable")

// allowedHostSuffixes are the hosts a download may be redirected to.
//
// A redirect cannot simply be refused the way the Ollama client refuses one:
// the hub serves large files by redirecting to its LFS/CDN hosts, so a pull
// genuinely needs to follow one. It is therefore constrained instead --
// https only, and only to the hub's own domains. The alternative, Go's default
// "follow anything", is exactly the bug that let another client in this repo
// replay a request to an arbitrary host.
var allowedHostSuffixes = []string{
	"huggingface.co",
	"hf.co",
}

// hubHostAllowed is the default redirect and request policy: https, and a host
// that is one of the hub's domains or a subdomain of one.
func hubHostAllowed(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("models: refusing %s://%s: model downloads must use https", u.Scheme, u.Host)
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range allowedHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return nil
		}
	}
	return fmt.Errorf("models: refusing to fetch from %q: the only host Kagaz downloads from is %s", host, Host)
}

// newHTTPClient builds the transport used for the one permitted egress.
//
// Redirects are checked hop by hop against the host policy rather than being
// blindly followed or blindly refused. HTTP_PROXY is honoured -- unlike the
// localhost-only Ollama client, where a proxy would carry document bytes off
// the machine, this client sends no document content at all (an empty request
// body and a public model id), and the responses are SHA256-verified, so a
// proxy cannot alter what lands on disk without being caught.
func (c *Client) newHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("models: stopped after %d redirects", maxRedirects)
			}
			return c.allowURL(req.URL)
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       60 * time.Second,
		},
	}
}

// allowURL applies the host policy, defaulting to hubHostAllowed.
func (c *Client) allowURL(u *url.URL) error {
	if c.hostAllowed != nil {
		return c.hostAllowed(u)
	}
	return hubHostAllowed(u)
}

// baseURL is the pinned endpoint, or a test override.
func (c *Client) baseURL() string {
	if c.endpoint != "" {
		return strings.TrimSuffix(c.endpoint, "/")
	}
	return DefaultEndpoint
}

func (c *Client) client() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	// Built per call rather than cached in a package var so a test-injected
	// host policy is always the one in force.
	return c.newHTTPClient()
}

// get issues a GET, applying the host policy to the initial URL as well as to
// every redirect. offset > 0 requests a byte range so an interrupted download
// resumes instead of restarting.
//
// The returned response's Body is the caller's to close.
func (c *Client) get(ctx context.Context, rawURL string, offset int64) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("models: invalid URL %q: %w", rawURL, err)
	}
	// The first hop is checked with the same policy as every later one: a
	// policy applied only to redirects would leave the entry point unguarded.
	if err := c.allowURL(u); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("models: fetching %s: %w", u.Redacted(), err)
	}
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusPartialContent:
		return resp, nil
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		drain(resp)
		return nil, errRangeNotSatisfiable
	case resp.StatusCode == http.StatusNotFound:
		drain(resp)
		return nil, fmt.Errorf("models: %s: not found (404)", u.Redacted())
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		drain(resp)
		return nil, fmt.Errorf("models: %s: %s -- gated or private repositories are not supported", u.Redacted(), resp.Status)
	default:
		drain(resp)
		return nil, fmt.Errorf("models: %s: %s", u.Redacted(), resp.Status)
	}
}

// drain reads and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}
