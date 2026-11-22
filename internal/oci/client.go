package oci

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avesst/garner/internal/config"
)

// Client reads images from a registry (section 19). Credentials go only
// to the registry's own host and to token endpoints on it or in
// auth_hosts; a redirect elsewhere, such as to a CDN for blobs, carries
// none.
type Client struct {
	name      string
	base      *url.URL
	username  string
	token     string
	authHosts []string
	http      *http.Client
	sem       chan struct{}

	mu sync.Mutex
	// bearer holds tokens from the registry's token endpoint by scope.
	bearer map[string]string
}

// NewClient builds a client for a configured registry.
func NewClient(r config.Registry) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(r.URL, "/"))
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	if r.Proxy != "" {
		u, err := url.Parse(r.Proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if r.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(r.CAFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificates found", r.CAFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	token := ""
	switch {
	case r.TokenEnv != "":
		token = os.Getenv(r.TokenEnv)
	case r.TokenFile != "":
		b, err := os.ReadFile(r.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("registry %s token: %w", r.Name, err)
		}
		token = strings.TrimSpace(string(b))
	}
	c := &Client{name: r.Name, base: base, username: r.Username, token: token, authHosts: r.AuthHosts,
		sem: make(chan struct{}, max(r.MaxConcurrent, 1)), bearer: map[string]string{}}
	c.http = &http.Client{Transport: tr, Timeout: 30 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if !c.sameHost(req.URL) {
				req.Header.Del("Authorization")
			}
			return nil
		}}
	return c, nil
}

func (c *Client) sameHost(u *url.URL) bool {
	return u.Scheme == c.base.Scheme && u.Host == c.base.Host
}

// mayAuth reports whether credentials may go to a token endpoint.
func (c *Client) mayAuth(u *url.URL) bool {
	return u.Scheme == "https" && (u.Host == c.base.Host || slices.Contains(c.authHosts, u.Host) ||
		slices.Contains(c.authHosts, u.Hostname()))
}

// StatusError is an unexpected response from the registry.
type StatusError struct {
	Status int
	URL    string
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: %d %s %s", e.URL, e.Status, http.StatusText(e.Status), e.Body)
}

// do sends a request to the registry, answering an authentication
// challenge once.
func (c *Client) do(ctx context.Context, method, path string, hdr http.Header) (*http.Response, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()
	ref, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	u := *c.base
	u.Path, u.RawQuery = ref.Path, ref.RawQuery
	// Tokens are kept by the pull scope of the image the path names, so
	// later requests for it start with one.
	scope := pullScope(ref.Path)
	limited := false
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		c.mu.Lock()
		bearer := c.bearer[scope]
		c.mu.Unlock()
		switch {
		case bearer != "":
			req.Header.Set("Authorization", "Bearer "+bearer)
		case scope == "basic" && c.token != "":
			req.SetBasicAuth(c.username, c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && !limited {
			// Wait once, as forge requests do; a longer limit, such as
			// Docker Hub's pull limit, fails this poll.
			limited = true
			wait := retryWait(resp)
			resp.Body.Close()
			if wait > maxRetryWait {
				return nil, fmt.Errorf("registry %s: rate limited for %s", c.name, wait.Round(time.Second))
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			attempt--
			continue
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		scheme, params := parseChallenge(challenge)
		switch strings.ToLower(scheme) {
		case "bearer":
			tok, err := c.fetchToken(ctx, params)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			c.bearer[scope] = tok
			c.mu.Unlock()
		case "basic":
			if c.token == "" {
				return nil, &StatusError{Status: http.StatusUnauthorized, URL: u.String(), Body: "the registry needs credentials"}
			}
			scope = "basic"
		default:
			return nil, &StatusError{Status: http.StatusUnauthorized, URL: u.String(), Body: "unsupported challenge " + challenge}
		}
	}
}

// maxRetryWait bounds how long a request waits for Retry-After before
// failing; the next poll retries.
const maxRetryWait = 2 * time.Minute

func retryWait(resp *http.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return time.Duration(n) * time.Second
		}
		if t, err := http.ParseTime(s); err == nil {
			return time.Until(t)
		}
	}
	return 10 * time.Second
}

// pullScope is the token scope of the image a /v2/ path names.
func pullScope(p string) string {
	rest := strings.TrimPrefix(p, "/v2/")
	for _, k := range []string{"/manifests/", "/blobs/", "/tags/list"} {
		if i := strings.LastIndex(rest, k); i > 0 {
			return "repository:" + rest[:i] + ":pull"
		}
	}
	return "registry"
}

// fetchToken gets a bearer token for a challenge.
func (c *Client) fetchToken(ctx context.Context, params map[string]string) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Host == "" {
		return "", fmt.Errorf("registry %s: bad token realm %q", c.name, params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if s := params["scope"]; s != "" {
		q.Set("scope", s)
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		if !c.mayAuth(realm) {
			return "", fmt.Errorf("registry %s: token endpoint %s is not the registry's host; list it in auth_hosts to send it the credentials", c.name, realm.Host)
		}
		user := c.username
		if user == "" {
			user = "garner"
		}
		req.SetBasicAuth(user, c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", &StatusError{Status: resp.StatusCode, URL: realm.Scheme + "://" + realm.Host + realm.Path, Body: string(b)}
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("registry %s token: %w", c.name, err)
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("registry %s: the token endpoint returned no token", c.name)
	}
	return tok, nil
}

// parseChallenge splits a WWW-Authenticate value into its scheme and
// parameters.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[k] = v[1:]
				break
			}
			params[k] = v[1 : end+1]
			rest = v[end+2:]
		} else {
			val, r, _ := strings.Cut(v, ",")
			params[k] = strings.TrimSpace(val)
			rest = r
		}
	}
	return scheme, params
}

func check(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	return &StatusError{Status: resp.StatusCode, URL: resp.Request.URL.Redacted(), Body: strings.TrimSpace(string(b))}
}

// Tags lists an image's tags, following the Link header across pages.
func (c *Client) Tags(ctx context.Context, image string) ([]string, error) {
	var out []string
	path := "/v2/" + image + "/tags/list"
	query := ""
	for page := 0; page < 1000; page++ {
		resp, err := c.do(ctx, http.MethodGet, path+query, nil)
		if err != nil {
			return nil, err
		}
		if err := check(resp); err != nil {
			return nil, err
		}
		var list TagList
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&list)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("tag list of %s: %w", image, err)
		}
		out = append(out, list.Tags...)
		next := nextLink(resp.Header.Get("Link"))
		if next == "" {
			return out, nil
		}
		u, err := url.Parse(next)
		if err != nil {
			return nil, err
		}
		path, query = u.Path, "?"+u.RawQuery
	}
	return nil, fmt.Errorf("tag list of %s: too many pages", image)
}

func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		if strings.Contains(part, `rel="next"`) {
			s, e := strings.Index(part, "<"), strings.Index(part, ">")
			if s >= 0 && e > s {
				return part[s+1 : e]
			}
		}
	}
	return ""
}

func manifestHeader() http.Header {
	return http.Header{"Accept": {strings.Join(MediaTypes(), ", ")}}
}

// Manifest fetches a manifest by tag or digest. A digest reference is
// checked against the content.
func (c *Client) Manifest(ctx context.Context, image, ref string) ([]byte, string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v2/"+image+"/manifests/"+ref, manifestHeader())
	if err != nil {
		return nil, "", err
	}
	if err := check(resp); err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxManifestSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > MaxManifestSize {
		return nil, "", fmt.Errorf("manifest %s:%s is larger than 4 MiB", image, ref)
	}
	if strings.HasPrefix(ref, "sha256:") && Digest(body) != ref {
		return nil, "", fmt.Errorf("manifest %s@%s has digest %s", image, ref, Digest(body))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// Resolve returns the digest a tag points at, with HEAD where the
// registry sends Docker-Content-Digest (a HEAD does not count against
// Docker Hub's pull limit), else from the manifest itself.
func (c *Client) Resolve(ctx context.Context, image, tag string) (string, error) {
	resp, err := c.do(ctx, http.MethodHead, "/v2/"+image+"/manifests/"+tag, manifestHeader())
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if err := check(resp); err != nil {
		return "", err
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		if _, err := ParseDigest(d); err == nil {
			return d, nil
		}
	}
	body, _, err := c.Manifest(ctx, image, tag)
	if err != nil {
		return "", err
	}
	return Digest(body), nil
}

// Blob opens a blob for reading. The caller checks its digest.
func (c *Client) Blob(ctx context.Context, image, digest string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v2/"+image+"/blobs/"+digest, nil)
	if err != nil {
		return nil, err
	}
	if err := check(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}
