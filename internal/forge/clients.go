package forge

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/avesst/garner/internal/config"
)

// releasePages bounds how far back a poll looks for unprocessed releases.
const releasePages = 10

// olderThan reports whether the oldest release on a page was published at
// or before since, so further pages are not needed.
func olderThan(rs []Release, since time.Time) bool {
	for _, r := range rs {
		if !r.PublishedAt.IsZero() && !r.PublishedAt.After(since) {
			return true
		}
	}
	return false
}

// --- GitHub and GitHub Enterprise Server ---

type github struct{ *httpClient }

func newGitHub(h *httpClient, f config.Forge) *github {
	if f.URL == "" {
		h.base, _ = url.Parse("https://api.github.com")
	} else {
		// GitHub Enterprise Server serves its API below /api/v3.
		h.base, _ = url.Parse(strings.TrimRight(f.URL, "/") + "/api/v3")
	}
	h.origin = &url.URL{Scheme: h.base.Scheme, Host: h.base.Host}
	return &github{h}
}

func (g *github) Name() string { return g.name }

type ghRepo struct {
	FullName string `json:"full_name"`
	Archived bool   `json:"archived"`
	Fork     bool   `json:"fork"`
}

func (g *github) ListRepos(ctx context.Context, org string, _ bool) ([]Repo, error) {
	q := url.Values{"per_page": {"100"}}
	list, _, err := pages[ghRepo](ctx, g.httpClient, g.api("/orgs/"+url.PathEscape(org)+"/repos", q), "", 100, nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		// Not an organization: list a user's repositories instead.
		list, _, err = pages[ghRepo](ctx, g.httpClient, g.api("/users/"+url.PathEscape(org)+"/repos", q), "", 100, nil)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Repo, len(list))
	for i, r := range list {
		out[i] = Repo{Path: r.FullName, Archived: r.Archived, Fork: r.Fork}
	}
	return out, nil
}

type ghRelease struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

func (g *github) Releases(ctx context.Context, repo string, since time.Time, etag string) ([]Release, string, error) {
	var out []Release
	_, tag, err := pages[ghRelease](ctx, g.httpClient, g.api("/repos/"+repo+"/releases", url.Values{"per_page": {"30"}}),
		etag, releasePages, func(page []ghRelease) bool {
			conv := make([]Release, len(page))
			for i, r := range page {
				rel := Release{ID: strconv.FormatInt(r.ID, 10), Tag: r.TagName, Draft: r.Draft,
					Prerelease: r.Prerelease, PublishedAt: r.PublishedAt}
				for _, a := range r.Assets {
					rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: a.URL, Size: a.Size,
						SHA256: strings.TrimPrefix(a.Digest, "sha256:")})
				}
				conv[i] = rel
			}
			out = append(out, conv...)
			return olderThan(conv, since)
		})
	return out, tag, err
}

// Download uses the API asset URL with Accept: application/octet-stream,
// which also works for private repositories. GitHub answers with a
// redirect to a storage host, which gets no credentials.
func (g *github) Download(ctx context.Context, a Asset) (io.ReadCloser, error) {
	return g.download(ctx, a.URL, http.Header{"Accept": {"application/octet-stream"}})
}

// --- Forgejo and Gitea ---

type forgejo struct{ http *httpClient }

func (f *forgejo) Name() string            { return f.http.name }
func (f *forgejo) RateLimitRemaining() int { return f.http.RateLimitRemaining() }

type fjRepo struct {
	FullName string `json:"full_name"`
	Archived bool   `json:"archived"`
	Fork     bool   `json:"fork"`
}

func (f *forgejo) ListRepos(ctx context.Context, org string, _ bool) ([]Repo, error) {
	list, _, err := pages[fjRepo](ctx, f.http, f.http.api("/orgs/"+url.PathEscape(org)+"/repos",
		url.Values{"limit": {"50"}}), "", 100, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Repo, len(list))
	for i, r := range list {
		out[i] = Repo{Path: r.FullName, Archived: r.Archived, Fork: r.Fork}
	}
	return out, nil
}

type fjRelease struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (f *forgejo) Releases(ctx context.Context, repo string, since time.Time, etag string) ([]Release, string, error) {
	var out []Release
	_, tag, err := pages[fjRelease](ctx, f.http, f.http.api("/repos/"+repo+"/releases", url.Values{"limit": {"30"}}),
		etag, releasePages, func(page []fjRelease) bool {
			conv := make([]Release, len(page))
			for i, r := range page {
				rel := Release{ID: strconv.FormatInt(r.ID, 10), Tag: r.TagName, Draft: r.Draft,
					Prerelease: r.Prerelease, PublishedAt: r.PublishedAt}
				for _, a := range r.Assets {
					rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: a.URL, Size: a.Size})
				}
				conv[i] = rel
			}
			out = append(out, conv...)
			return olderThan(conv, since)
		})
	return out, tag, err
}

func (f *forgejo) Download(ctx context.Context, a Asset) (io.ReadCloser, error) {
	return f.http.download(ctx, a.URL, nil)
}

// --- GitLab ---

type gitlab struct{ http *httpClient }

func (g *gitlab) Name() string            { return g.http.name }
func (g *gitlab) RateLimitRemaining() int { return g.http.RateLimitRemaining() }

type glProject struct {
	PathWithNamespace string `json:"path_with_namespace"`
	Archived          bool   `json:"archived"`
	ForkedFrom        *struct {
		ID int64 `json:"id"`
	} `json:"forked_from_project"`
}

func (g *gitlab) ListRepos(ctx context.Context, group string, recursive bool) ([]Repo, error) {
	q := url.Values{"per_page": {"100"}, "include_subgroups": {strconv.FormatBool(recursive)}}
	list, _, err := pages[glProject](ctx, g.http, g.http.api("/groups/"+escapeAll(group)+"/projects", q), "", 100, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Repo, len(list))
	for i, p := range list {
		out[i] = Repo{Path: p.PathWithNamespace, Archived: p.Archived, Fork: p.ForkedFrom != nil}
	}
	return out, nil
}

type glRelease struct {
	TagName         string    `json:"tag_name"`
	ReleasedAt      time.Time `json:"released_at"`
	UpcomingRelease bool      `json:"upcoming_release"`
	Assets          struct {
		Links []struct {
			Name           string `json:"name"`
			URL            string `json:"url"`
			DirectAssetURL string `json:"direct_asset_url"`
		} `json:"links"`
	} `json:"assets"`
}

func (g *gitlab) Releases(ctx context.Context, repo string, since time.Time, etag string) ([]Release, string, error) {
	var out []Release
	_, tag, err := pages[glRelease](ctx, g.http, g.http.api("/projects/"+escapeAll(repo)+"/releases",
		url.Values{"per_page": {"30"}}), etag, releasePages, func(page []glRelease) bool {
		conv := make([]Release, len(page))
		for i, r := range page {
			// GitLab has no release ids or drafts; a tag names a release, and
			// an upcoming release (released_at in the future) is not out yet.
			rel := Release{ID: r.TagName, Tag: r.TagName, Draft: r.UpcomingRelease, PublishedAt: r.ReleasedAt}
			for _, l := range r.Assets.Links {
				u := l.DirectAssetURL
				if u == "" {
					u = l.URL
				}
				rel.Assets = append(rel.Assets, Asset{Name: l.Name, URL: u})
			}
			conv[i] = rel
		}
		out = append(out, conv...)
		return olderThan(conv, since)
	})
	return out, tag, err
}

func (g *gitlab) Download(ctx context.Context, a Asset) (io.ReadCloser, error) {
	return g.http.download(ctx, a.URL, nil)
}

// escapeAll escapes a GitLab path for use as an id, slashes included.
func escapeAll(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "/", "%2F")
}
