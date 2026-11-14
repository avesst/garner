// Package forge talks to GitHub, GitHub Enterprise Server, Forgejo/Gitea
// and GitLab release APIs (design section 7).
package forge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avesst/garner/internal/config"
)

// Repo is a repository or GitLab project.
type Repo struct {
	// Path is owner/name, or the full GitLab project path.
	Path     string
	Archived bool
	Fork     bool
}

// Release is one release of a repository.
type Release struct {
	ID          string
	Tag         string
	Draft       bool
	Prerelease  bool
	PublishedAt time.Time
	Assets      []Asset
}

// Asset is a downloadable release file.
type Asset struct {
	Name string
	URL  string
	Size int64
	// SHA256 is the forge-provided digest, lowercase hex, or "".
	SHA256 string
}

// ErrNotModified is returned when an ETag matched.
var ErrNotModified = errors.New("not modified")

// Client is one configured forge.
type Client interface {
	// Name is the configured forge name.
	Name() string
	// ListRepos lists the repositories of an organization (GitHub,
	// Forgejo) or group (GitLab).
	ListRepos(ctx context.Context, owner string, recursive bool) ([]Repo, error)
	// Releases returns a repository's releases, newest first, up to
	// enough pages to reach releases published at or before since. The
	// first page uses etag; ErrNotModified means nothing changed.
	Releases(ctx context.Context, repo string, since time.Time, etag string) ([]Release, string, error)
	// Download opens an asset. Credentials are sent only to the forge's
	// own host.
	Download(ctx context.Context, a Asset) (io.ReadCloser, error)
	// RateLimitRemaining is the last reported remaining quota, or -1.
	RateLimitRemaining() int
}

// New creates the client for a configured forge.
func New(f config.Forge) (Client, error) {
	h, err := newHTTP(f)
	if err != nil {
		return nil, err
	}
	setBase := func(suffix string) error {
		b, err := url.Parse(strings.TrimRight(f.URL, "/") + suffix)
		if err != nil {
			return err
		}
		h.base = b
		h.origin = &url.URL{Scheme: b.Scheme, Host: b.Host}
		return nil
	}
	switch f.Type {
	case "github":
		return newGitHub(h, f), nil
	case "forgejo", "gitea":
		if err := setBase("/api/v1"); err != nil {
			return nil, err
		}
		return &forgejo{http: h}, nil
	case "gitlab":
		if err := setBase("/api/v4"); err != nil {
			return nil, err
		}
		return &gitlab{http: h}, nil
	}
	return nil, fmt.Errorf("unknown forge type %q", f.Type)
}

// httpClient is the shared HTTP layer: credentials only for the forge's
// own origin, at most max_concurrent requests, rate-limit tracking.
type httpClient struct {
	name      string
	base      *url.URL // API base
	origin    *url.URL // scheme, host and port that may receive credentials
	client    *http.Client
	setAuth   func(*http.Request)
	sem       chan struct{}
	remaining chan int
}

func readToken(f config.Forge) (string, error) {
	switch {
	case f.TokenEnv != "":
		return os.Getenv(f.TokenEnv), nil
	case f.TokenFile != "":
		b, err := os.ReadFile(f.TokenFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

func newHTTP(f config.Forge) (*httpClient, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if f.Proxy != "" {
		u, err := url.Parse(f.Proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if f.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(f.CAFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificates found", f.CAFile)
		}
		// Verification stays on; ca_file only adds trust (section 7).
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	token, err := readToken(f)
	if err != nil {
		return nil, fmt.Errorf("forge %s token: %w", f.Name, err)
	}
	h := &httpClient{name: f.Name, sem: make(chan struct{}, max(f.MaxConcurrent, 1)), remaining: make(chan int, 1)}
	h.remaining <- -1
	h.client = &http.Client{Transport: &authTransport{h: h, next: tr}, Timeout: 30 * time.Minute}
	h.setAuth = func(*http.Request) {}
	if token != "" {
		switch f.Type {
		case "gitlab":
			h.setAuth = func(r *http.Request) { r.Header.Set("PRIVATE-TOKEN", token) }
		case "github":
			h.setAuth = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
		default:
			h.setAuth = func(r *http.Request) { r.Header.Set("Authorization", "token "+token) }
		}
	}
	return h, nil
}

// authTransport adds credentials per request, so a redirect to another
// host (a CDN, an external asset link) never carries them.
type authTransport struct {
	h    *httpClient
	next http.RoundTripper
}

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if sameOrigin(r.URL, t.h.origin) {
		r = r.Clone(r.Context())
		t.h.setAuth(r)
	}
	return t.next.RoundTrip(r)
}

func sameOrigin(a, b *url.URL) bool {
	return b != nil && a.Scheme == b.Scheme && a.Hostname() == b.Hostname() && port(a) == port(b)
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func (h *httpClient) RateLimitRemaining() int {
	v := <-h.remaining
	h.remaining <- v
	return v
}

func (h *httpClient) recordRateLimit(resp *http.Response) {
	for _, k := range []string{"X-RateLimit-Remaining", "RateLimit-Remaining"} {
		if v := resp.Header.Get(k); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				<-h.remaining
				h.remaining <- n
			}
			return
		}
	}
}

// maxRetryWait bounds how long a request waits for Retry-After or a
// rate-limit reset before failing; the next poll retries.
const maxRetryWait = 2 * time.Minute

// do sends a request, honouring Retry-After and rate-limit resets once.
func (h *httpClient) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-h.sem }()
	for attempt := 0; ; attempt++ {
		resp, err := h.client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		h.recordRateLimit(resp)
		limited := resp.StatusCode == http.StatusTooManyRequests ||
			(resp.StatusCode == http.StatusForbidden && h.RateLimitRemaining() == 0)
		if !limited || attempt > 0 {
			return resp, nil
		}
		wait := retryWait(resp)
		resp.Body.Close()
		if wait > maxRetryWait {
			return nil, fmt.Errorf("forge %s: rate limited for %s", h.name, wait.Round(time.Second))
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func retryWait(resp *http.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return time.Duration(n) * time.Second
		}
		if t, err := http.ParseTime(s); err == nil {
			return time.Until(t)
		}
	}
	for _, k := range []string{"X-RateLimit-Reset", "RateLimit-Reset"} {
		if s := resp.Header.Get(k); s != "" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return time.Until(time.Unix(n, 0))
			}
		}
	}
	return 10 * time.Second
}

// getJSON fetches an API URL into out. With an etag it sends
// If-None-Match and returns ErrNotModified on 304. It returns the
// response's ETag and the URL of the next page, if any.
func (h *httpClient) getJSON(ctx context.Context, u, etag string, out any) (string, string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := h.do(ctx, req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return etag, "", ErrNotModified
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("GET %s: HTTP %d: %s", redact(u), resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return "", "", fmt.Errorf("GET %s: %w", redact(u), err)
	}
	return resp.Header.Get("ETag"), nextLink(resp.Header.Get("Link")), nil
}

var linkRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

func nextLink(h string) string {
	if m := linkRE.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

// redact drops the query, which some forges accept tokens in.
func redact(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// download opens u, with extra headers.
func (h *httpClient) download(ctx context.Context, u string, header http.Header) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := h.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		msg := fmt.Sprintf("download %s: HTTP %d", redact(u), resp.StatusCode)
		if !sameOrigin(resp.Request.URL, h.origin) {
			msg += " (fetched without credentials: the link points outside the forge)"
		}
		return nil, errors.New(msg)
	}
	return resp.Body, nil
}

// api builds an API URL; path must already be escaped.
func (h *httpClient) api(path string, query url.Values) string {
	u := strings.TrimRight(h.base.String(), "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// pages fetches up to maxPages of a listing; stop reports whether the
// page just decoded is enough.
func pages[T any](ctx context.Context, h *httpClient, first, etag string, maxPages int, stop func([]T) bool) ([]T, string, error) {
	var all []T
	u := first
	newETag := ""
	for i := 0; u != "" && i < maxPages; i++ {
		var page []T
		e := ""
		if i == 0 {
			e = etag
		}
		tag, next, err := h.getJSON(ctx, u, e, &page)
		if err != nil {
			return nil, etag, err
		}
		if i == 0 {
			newETag = tag
		}
		all = append(all, page...)
		if len(page) == 0 || (stop != nil && stop(page)) {
			break
		}
		u = next
	}
	return all, newETag, nil
}
