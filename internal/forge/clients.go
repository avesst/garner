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

// later returns the later of two times; a zero time counts as unknown.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
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
		Name      string    `json:"name"`
		URL       string    `json:"url"`
		Size      int64     `json:"size"`
		Digest    string    `json:"digest"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"assets"`
}

func (g *github) Releases(ctx context.Context, repo string, since time.Time, etag string) ([]Release, string, error) {
	var out []Release
	_, tag, err := pages[ghRelease](ctx, g.httpClient, g.api("/repos/"+repo+"/releases", url.Values{"per_page": {"30"}}),
		etag, releasePages, func(page []ghRelease) bool {
			conv := make([]Release, len(page))
			for i, r := range page {
				conv[i] = r.release()
			}
			out = append(out, conv...)
			return olderThan(conv, since)
		})
	return out, tag, err
}

func (g *github) Release(ctx context.Context, repo, id string) (Release, bool, error) {
	var r ghRelease
	found, err := g.getOne(ctx, g.api("/repos/"+repo+"/releases/"+url.PathEscape(id), nil), &r)
	if !found {
		return Release{}, false, err
	}
	return r.release(), true, nil
}

func (r ghRelease) release() Release {
	rel := Release{ID: strconv.FormatInt(r.ID, 10), Tag: r.TagName, Draft: r.Draft,
		Prerelease: r.Prerelease, PublishedAt: r.PublishedAt, AssetTimes: true}
	for _, a := range r.Assets {
		// updated_at moves on a rename, which can turn an old file into a
		// candidate.
		rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: a.URL, Size: a.Size,
			SHA256: strings.TrimPrefix(a.Digest, "sha256:"), CreatedAt: later(a.CreatedAt, a.UpdatedAt)})
	}
	return rel
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
		Name      string    `json:"name"`
		Size      int64     `json:"size"`
		URL       string    `json:"browser_download_url"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"assets"`
}

func (f *forgejo) Releases(ctx context.Context, repo string, since time.Time, etag string) ([]Release, string, error) {
	var out []Release
	_, tag, err := pages[fjRelease](ctx, f.http, f.http.api("/repos/"+repo+"/releases", url.Values{"limit": {"30"}}),
		etag, releasePages, func(page []fjRelease) bool {
			conv := make([]Release, len(page))
			for i, r := range page {
				conv[i] = r.release()
			}
			out = append(out, conv...)
			return olderThan(conv, since)
		})
	return out, tag, err
}

func (f *forgejo) Release(ctx context.Context, repo, id string) (Release, bool, error) {
	var r fjRelease
	found, err := f.http.getOne(ctx, f.http.api("/repos/"+repo+"/releases/"+url.PathEscape(id), nil), &r)
	if !found {
		return Release{}, false, err
	}
	return r.release(), true, nil
}

func (r fjRelease) release() Release {
	rel := Release{ID: strconv.FormatInt(r.ID, 10), Tag: r.TagName, Draft: r.Draft,
		Prerelease: r.Prerelease, PublishedAt: r.PublishedAt, AssetTimes: true}
	for _, a := range r.Assets {
		rel.Assets = append(rel.Assets, Asset{Name: a.Name, URL: a.URL, Size: a.Size, CreatedAt: a.CreatedAt})
	}
	return rel
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
	CreatedAt       time.Time `json:"created_at"`
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
			conv[i] = r.release()
		}
		out = append(out, conv...)
		return olderThan(conv, since)
	})
	return out, tag, err
}

// Release looks a release up by its tag, and finds it only if it is still
// the one created at the time its ID records. A bare tag, the ID from
// before IDs carried the creation time, finds the release under its
// current ID.
func (g *gitlab) Release(ctx context.Context, repo, id string) (Release, bool, error) {
	tag, _, _ := strings.Cut(id, ":")
	var r glRelease
	found, err := g.http.getOne(ctx, g.http.api("/projects/"+escapeAll(repo)+"/releases/"+url.PathEscape(tag), nil), &r)
	if !found {
		return Release{}, false, err
	}
	rel := r.release()
	if strings.Contains(id, ":") && rel.ID != id {
		return Release{}, false, nil // deleted and re-created since
	}
	return rel, true, nil
}

// release converts a GitLab release. GitLab has no release ids or drafts.
// A tag names a release, so the ID adds the creation time: a release
// deleted and re-created under the same tag is a new release. Git refs
// cannot contain ':', so the tag ends at the first one. An upcoming
// release (released_at in the future) is not out yet. The caller may set
// released_at to a past date (a historical release), so the server's
// created_at bounds it: a backdated release must not skip
// min_release_age. The listing stays in GitLab's released_at order, which
// places an upcoming release where it comes out; a backdated one sorts far
// back and may not be listed, which publishes nothing early. Links carry
// no time.
func (r glRelease) release() Release {
	rel := Release{ID: r.TagName + ":" + r.CreatedAt.UTC().Format(time.RFC3339Nano), Tag: r.TagName,
		Draft: r.UpcomingRelease, PublishedAt: later(r.CreatedAt, r.ReleasedAt), FormerID: r.TagName}
	for _, l := range r.Assets.Links {
		u := l.DirectAssetURL
		if u == "" {
			u = l.URL
		}
		rel.Assets = append(rel.Assets, Asset{Name: l.Name, URL: u})
	}
	return rel
}

func (g *gitlab) Download(ctx context.Context, a Asset) (io.ReadCloser, error) {
	return g.http.download(ctx, a.URL, nil)
}

// escapeAll escapes a GitLab path for use as an id, slashes included.
func escapeAll(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "/", "%2F")
}
