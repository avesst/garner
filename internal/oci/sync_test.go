package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/store"
)

// upstream is a fake registry with Bearer auth, a token endpoint on
// another host and blobs redirected to a CDN host.
type upstream struct {
	t        *testing.T
	reg      *httptest.Server
	auth     *httptest.Server
	cdn      *httptest.Server
	mu       sync.Mutex
	tags     map[string]string // tag -> digest
	content  map[string][]byte // digest -> manifest or blob
	types    map[string]string // manifest digest -> media type
	needAuth string            // basic credentials the token endpoint wants, "" for anonymous
	leaked   bool              // the CDN got an Authorization header
	corrupt  string            // digest whose blob the CDN serves wrong
	gets     int               // manifest GETs served
	missing  string            // digest the CDN answers 404 for, as a transient error
	blobGets int               // blob requests the CDN got
}

const testToken = "upstream-token"

func newUpstream(t *testing.T) *upstream {
	u := &upstream{t: t, tags: map[string]string{}, content: map[string][]byte{}, types: map[string]string{}}
	u.cdn = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			u.leaked = true
		}
		u.blobGets++
		d := strings.TrimPrefix(r.URL.Path, "/")
		b, ok := u.content[d]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if d == u.corrupt {
			b = append([]byte("x"), b...)
		}
		if d == u.missing {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	u.auth = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.needAuth != "" {
			user, pass, ok := r.BasicAuth()
			if !ok || user+":"+pass != u.needAuth {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"token": testToken})
	}))
	u.reg = httptest.NewTLSServer(http.HandlerFunc(u.serve))
	t.Cleanup(func() { u.reg.Close(); u.auth.Close(); u.cdn.Close() })
	return u
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+u.auth.URL+`/token",service="fake",scope="repository:org/tool:pull"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	rt, ok := parseRoute(r.URL.Path)
	if !ok || rt.name != "org/tool" {
		http.NotFound(w, r)
		return
	}
	switch rt.kind {
	case "tags":
		var list TagList
		list.Name = rt.name
		for t := range u.tags {
			list.Tags = append(list.Tags, t)
		}
		json.NewEncoder(w).Encode(list)
	case "manifests":
		d := rt.ref
		if !strings.HasPrefix(d, "sha256:") {
			d = u.tags[d]
		}
		b, ok := u.content[d]
		if !ok || u.types[d] == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", u.types[d])
		w.Header().Set("Docker-Content-Digest", d)
		if r.Method == http.MethodGet {
			u.gets++
			w.Write(b)
		}
	case "blobs":
		http.Redirect(w, r, u.cdn.URL+"/"+rt.ref, http.StatusTemporaryRedirect)
	default:
		http.NotFound(w, r)
	}
}

func (u *upstream) add(tag string, img *image) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.content[Digest(img.config)] = img.config
	for _, l := range img.layers {
		u.content[Digest(l)] = l
	}
	u.content[img.digest] = img.manifest
	var m Manifest
	json.Unmarshal(img.manifest, &m)
	u.types[img.digest] = m.MediaType
	u.tags[tag] = img.digest
}

// caFile writes the test servers' certificate (one for all of them).
func (u *upstream) caFile(dir string) string {
	p := filepath.Join(dir, "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: u.reg.Certificate().Raw})
	if err := os.WriteFile(p, pemBytes, 0o644); err != nil {
		u.t.Fatal(err)
	}
	return p
}

func (f *fixture) syncer() *Syncer {
	return &Syncer{Store: f.store, Blobs: f.reg.Blobs, Ingester: f.reg.Ingester, Now: func() time.Time { return f.now }}
}

func (f *fixture) configureSource(u *upstream, src config.Source, reg config.Registry) {
	reg.Name, reg.URL, reg.CAFile, reg.MaxConcurrent = "up", u.reg.URL, u.caFile(f.root), 2
	f.cfg.Registries = []config.Registry{reg}
	src.Name, src.Registry, src.Image = "tool", "up", "org/tool"
	f.cfg.Sources = []config.Source{src}
}

func (f *fixture) syncOnce() ([]string, error) {
	f.t.Helper()
	res, err := f.syncer().SyncImages(context.Background(), f.cfg.Sources[0], f.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var deferred []string
	for _, d := range res.Deferred {
		deferred = append(deferred, d.Tag)
	}
	return deferred, err
}

func TestSyncCopiesMatchingTags(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	v1 := newImage(MediaOCIManifest, "1", "base", "v1")
	u.add("v1", v1)
	u.add("dev", newImage(MediaOCIManifest, "dev", "dev layer"))
	f.configureSource(u, config.Source{Tags: []string{"v*"}, As: "tool"}, config.Registry{})
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if resp, body := f.pull("tool", "v1"); resp.StatusCode != http.StatusOK || !bytes.Equal(body, v1.manifest) {
		t.Fatalf("pull synced v1: %d", resp.StatusCode)
	}
	if resp, _ := f.pull("tool", "dev"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("dev does not match v*, but is served: %d", resp.StatusCode)
	}
	if u.leaked {
		t.Error("the CDN received credentials")
	}
	// The source owns the name now.
	o, err := f.store.Owner(context.Background(), "oci", "tool")
	if err != nil || o.Owner != "source:tool" {
		t.Errorf("owner %+v, %v", o, err)
	}

	// The tag moves upstream; garner follows and keeps the old digest.
	v2 := newImage(MediaOCIManifest, "2", "base", "v2")
	u.add("v1", v2)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if _, body := f.pull("tool", "v1"); !bytes.Equal(body, v2.manifest) {
		t.Error("v1 did not move")
	}
	if resp, _ := f.pull("tool", v1.digest); resp.StatusCode != http.StatusOK {
		t.Errorf("old digest: %d", resp.StatusCode)
	}
	f.verify()
}

func TestSyncCredentialsOnlyToAllowedHosts(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	u.needAuth = "bot:" + "s3cret"
	u.add("v1", newImage(MediaOCIManifest, "1", "l"))
	os.WriteFile(filepath.Join(f.root, "token"), []byte("s3cret\n"), 0o600)
	reg := config.Registry{Username: "bot", TokenFile: filepath.Join(f.root, "token")}
	f.configureSource(u, config.Source{Tags: []string{"*"}}, reg)
	if _, err := f.syncOnce(); err == nil || !strings.Contains(err.Error(), "auth_hosts") {
		t.Fatalf("token endpoint on another host got credentials: err %v", err)
	}
	authURL, _ := url.Parse(u.auth.URL)
	reg.AuthHosts = []string{authURL.Host}
	f.configureSource(u, config.Source{Tags: []string{"*"}}, reg)
	if _, err := f.syncOnce(); err != nil {
		t.Fatalf("with auth_hosts: %v", err)
	}
	if u.leaked {
		t.Error("the CDN received credentials")
	}
}

func TestSyncWaitsForMinReleaseAge(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	u.add("v1", newImage(MediaOCIManifest, "1", "l"))
	age := 48 * time.Hour
	f.configureSource(u, config.Source{Tags: []string{"*"}, MinReleaseAge: &age}, config.Registry{})
	deferred, err := f.syncOnce()
	if err != nil || len(deferred) != 1 {
		t.Fatalf("first sync: deferred %v, err %v", deferred, err)
	}
	f.now = f.now.Add(24 * time.Hour)
	if deferred, _ := f.syncOnce(); len(deferred) != 1 {
		t.Fatalf("after a day: deferred %v", deferred)
	}
	f.now = f.now.Add(25 * time.Hour)
	if deferred, err := f.syncOnce(); err != nil || len(deferred) != 0 {
		t.Fatalf("after the age: deferred %v, err %v", deferred, err)
	}
	f.publish()
	if resp, _ := f.pull("org/tool", "v1"); resp.StatusCode != http.StatusOK {
		t.Errorf("pull: %d", resp.StatusCode)
	}

	// An override lets a younger digest through.
	u.add("v2", newImage(MediaOCIManifest, "2", "m"))
	if deferred, _ := f.syncOnce(); len(deferred) != 1 {
		t.Fatalf("v2: deferred %v", deferred)
	}
	if err := f.store.AddAgeOverride(context.Background(), "tool", "org/tool", "v2", "admin", f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if deferred, err := f.syncOnce(); err != nil || len(deferred) != 0 {
		t.Fatalf("with override: deferred %v, err %v", deferred, err)
	}
}

func TestSyncRejectsCorruptBlob(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	img := newImage(MediaOCIManifest, "1", "good layer")
	u.add("v1", img)
	u.corrupt = Digest(img.layers[0])
	f.configureSource(u, config.Source{Tags: []string{"*"}}, config.Registry{})
	if _, err := f.syncOnce(); err == nil || !strings.Contains(err.Error(), "downloaded with digest") {
		t.Fatalf("corrupt blob: err %v", err)
	}
	f.publish()
	if resp, _ := f.pull("org/tool", "v1"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("image with a corrupt layer is served: %d", resp.StatusCode)
	}
	u.corrupt = ""
	if _, err := f.syncOnce(); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestSyncOversizeLayerNotDownloaded(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	img := newImage(MediaOCIManifest, "1", strings.Repeat("big layer ", 100))
	u.add("v1", img)
	f.cfg.Storage.MaxPackageSize = 500
	f.configureSource(u, config.Source{Tags: []string{"*"}}, config.Registry{})
	for range 2 {
		if _, err := f.syncOnce(); err == nil || !strings.Contains(err.Error(), "max_package_size") {
			t.Fatalf("oversize layer: err %v", err)
		}
	}
	if u.blobGets != 0 {
		t.Errorf("the CDN got %d blob requests for an image with a too-large layer", u.blobGets)
	}
	f.cfg.Storage.MaxPackageSize = 2 << 30
	if _, err := f.syncOnce(); err != nil {
		t.Fatalf("after raising the limit: %v", err)
	}
	f.publish()
	if resp, body := f.pull("org/tool", "v1"); resp.StatusCode != http.StatusOK {
		t.Errorf("pull after raising the limit: %d %s", resp.StatusCode, body)
	}
}

func TestSyncMultiArch(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	amd := newImage(MediaDockerManifest, "amd", "a")
	arm := newImage(MediaDockerManifest, "arm", "b")
	u.add("child-a", amd)
	u.add("child-b", arm)
	idx, _ := json.Marshal(Manifest{SchemaVersion: 2, MediaType: MediaDockerList, Manifests: []Descriptor{
		{MediaType: MediaDockerManifest, Digest: amd.digest, Size: int64(len(amd.manifest)), Platform: &Platform{OS: "linux", Architecture: "amd64"}},
		{MediaType: MediaDockerManifest, Digest: arm.digest, Size: int64(len(arm.manifest)), Platform: &Platform{OS: "linux", Architecture: "arm64"}},
	}})
	u.mu.Lock()
	u.content[Digest(idx)], u.types[Digest(idx)], u.tags["1.0"] = idx, MediaDockerList, Digest(idx)
	u.mu.Unlock()
	f.configureSource(u, config.Source{Tags: []string{"1.*"}}, config.Registry{})
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	resp, body := f.pull("org/tool", "1.0")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, idx) || resp.Header.Get("Content-Type") != MediaDockerList {
		t.Fatalf("pull index: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	f.verify()
}

// A rollback or a delete of a synced image is not undone by the next poll:
// a processed digest that is no longer placed stays out.
func TestSyncKeepsRollbackAndDelete(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	v1 := newImage(MediaOCIManifest, "1", "a")
	v2 := newImage(MediaOCIManifest, "2", "b")
	u.add("latest", v1)
	f.configureSource(u, config.Source{Tags: []string{"latest"}}, config.Registry{})
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	s1, err := f.pub.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u.add("latest", v2)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if _, err := f.pub.Rollback(context.Background(), s1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if _, body := f.pull("org/tool", "latest"); !bytes.Equal(body, v1.manifest) {
		t.Error("the poll after a rollback moved latest back to v2")
	}

	// Deleting the digest latest points at keeps it deleted.
	resp, body := f.do("DELETE", "/v2/org/tool/manifests/"+v1.digest, nil)
	f.want(resp, body, http.StatusAccepted)
	u.add("latest", v1)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if resp, _ := f.pull("org/tool", v1.digest); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted digest synced again: %d", resp.StatusCode)
	}

	// A new digest is synced as usual.
	v3 := newImage(MediaOCIManifest, "3", "c")
	u.add("latest", v3)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if _, body := f.pull("org/tool", "latest"); !bytes.Equal(body, v3.manifest) {
		t.Error("a new digest was not synced")
	}
}

// A registry source without its own retention keeps oci.retention
// untagged digests, not defaults.retention.
func TestSyncRetentionDefault(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention *int
		want      int // digests served of 5
	}{
		{"oci.retention", nil, 3},
		{"source retention", ptr(1), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.cfg.OCI.Retention = 2
			f.cfg.Defaults.Retention = 4
			u := newUpstream(t)
			f.configureSource(u, config.Source{Tags: []string{"latest"}, Retention: tc.retention}, config.Registry{})
			var imgs []*image
			for i := range 5 {
				img := newImage(MediaOCIManifest, fmt.Sprint(i), fmt.Sprint("l", i))
				imgs = append(imgs, img)
				u.add("latest", img)
				f.now = f.now.Add(time.Second)
				if _, err := f.syncOnce(); err != nil {
					t.Fatal(err)
				}
				f.publish()
			}
			served := 0
			for _, img := range imgs {
				if resp, _ := f.pull("org/tool", img.digest); resp.StatusCode == http.StatusOK {
					served++
				}
			}
			if served != tc.want {
				t.Errorf("%d digests served, want %d", served, tc.want)
			}
		})
	}
}

// A short Retry-After is waited out; a long one fails the poll.
func TestSyncRateLimit(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	u.add("v1", newImage(MediaOCIManifest, "1", "l"))
	var limits []string
	inner := u.reg.Config.Handler
	u.reg.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		var next string
		if len(limits) > 0 && strings.HasSuffix(r.URL.Path, "/tags/list") {
			next, limits = limits[0], limits[1:]
		}
		u.mu.Unlock()
		if next != "" {
			w.Header().Set("Retry-After", next)
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		inner.ServeHTTP(w, r)
	})
	f.configureSource(u, config.Source{Tags: []string{"*"}}, config.Registry{})
	limits = []string{"1"}
	if _, err := f.syncOnce(); err != nil {
		t.Fatalf("after a 1s Retry-After: %v", err)
	}
	limits = []string{"3600"}
	if _, err := f.syncOnce(); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("a 1h Retry-After: %v", err)
	}
}

func TestSyncSharesClientPerRegistry(t *testing.T) {
	s := &Syncer{}
	r := config.Registry{Name: "r", URL: "https://reg.example", MaxConcurrent: 2}
	a, _ := s.client(r)
	b, _ := s.client(r)
	r.MaxConcurrent = 3
	c, _ := s.client(r)
	if a != b || a == c {
		t.Errorf("clients: same config shared %v, changed config rebuilt %v", a == b, a != c)
	}
}

// Rolling back keeps the rolled-back tag even when the image it rolled away
// from is still served under another tag; an admin's tag delete holds too.
func TestSyncRollbackHoldsWithOtherTags(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	v1 := newImage(MediaOCIManifest, "1", "a")
	v2 := newImage(MediaOCIManifest, "2", "b")
	u.add("latest", v1)
	u.add("v1", v1)
	u.add("v2", v2)
	f.configureSource(u, config.Source{Tags: []string{"latest", "v*"}}, config.Registry{})
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	s1, err := f.pub.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u.add("latest", v2)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if _, body := f.pull("org/tool", "latest"); !bytes.Equal(body, v2.manifest) {
		t.Fatal("latest did not move to v2")
	}
	if _, err := f.pub.Rollback(context.Background(), s1.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := f.syncOnce(); err != nil {
			t.Fatal(err)
		}
		f.publish()
	}
	if _, body := f.pull("org/tool", "latest"); !bytes.Equal(body, v1.manifest) {
		t.Error("a poll after the rollback moved latest back to v2, which v2 kept active")
	}

	resp, body := f.do("DELETE", "/v2/org/tool/manifests/v2", nil)
	f.want(resp, body, http.StatusAccepted)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if resp, _ := f.pull("org/tool", "v2"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a poll re-created the deleted tag v2: %d", resp.StatusCode)
	}

	// Once upstream moves latest on, garner follows again.
	v3 := newImage(MediaOCIManifest, "3", "c")
	u.add("latest", v3)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if _, body := f.pull("org/tool", "latest"); !bytes.Equal(body, v3.manifest) {
		t.Error("latest did not follow upstream to v3")
	}
}

// A tag upstream moves more often than min_release_age still syncs: the
// newest digest that is old enough is applied.
func TestSyncMinAgeWithMovingTag(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	age := 48 * time.Hour
	f.configureSource(u, config.Source{Tags: []string{"latest"}, MinReleaseAge: &age}, config.Registry{})
	var imgs []*image
	for day := range 10 {
		img := newImage(MediaOCIManifest, fmt.Sprint(day), fmt.Sprint("layer ", day))
		imgs = append(imgs, img)
		u.add("latest", img)
		for range 24 / 6 {
			if _, err := f.syncOnce(); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(6 * time.Hour)
		}
	}
	f.publish()
	_, body := f.pull("org/tool", "latest")
	// After day 9's push, the newest digest at least 48h old is day 7's.
	if !bytes.Equal(body, imgs[7].manifest) {
		got := -1
		for i, img := range imgs {
			if bytes.Equal(body, img.manifest) {
				got = i
			}
		}
		t.Errorf("latest is day %d's image, want day 7's", got)
	}
	cands, err := f.store.TagCandidates(context.Background(), store.TagKey{Source: "tool", Repo: "org/tool", Name: "org/tool", Tag: "latest"})
	if err != nil || len(cands) != 2 {
		t.Errorf("candidates left: %d (%v), want the 2 newer than the applied one", len(cands), err)
	}
	deferred, _ := f.syncOnce()
	if len(deferred) != 1 {
		t.Errorf("deferred %v, want latest", deferred)
	}
}

// A held image is not fetched again on later polls, and its blobs are not
// staged, until it is approved; then the tag follows without a fetch.
func TestSyncHeldNotRefetched(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	img := newImage(MediaOCIManifest, "1", "held layer")
	u.add("v1", img)
	manual := "manual"
	f.configureSource(u, config.Source{Tags: []string{"v1"}, Approval: &manual}, config.Registry{})
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	u.mu.Lock()
	before := u.gets
	u.mu.Unlock()
	f.now = f.now.Add(25 * time.Hour) // past the stage of the first fetch
	for range 3 {
		if _, err := f.syncOnce(); err != nil {
			t.Fatal(err)
		}
	}
	u.mu.Lock()
	after := u.gets
	u.mu.Unlock()
	if after != before {
		t.Errorf("held image fetched %d more times", after-before)
	}
	layer := strings.TrimPrefix(Digest(img.layers[0]), "sha256:")
	if staged, _ := f.store.Staged(context.Background(), layer, f.now); staged {
		t.Error("held image's layer is staged, so pushes could use it")
	}

	pkgs, _ := f.store.Placed(context.Background(), store.PackageFilter{Name: "org/tool"})
	if len(pkgs) != 1 || pkgs[0].State != store.Held {
		t.Fatalf("packages %v", pkgs)
	}
	if _, err := f.store.SetState(context.Background(), pkgs[0].ID, pkgs[0].Distribution, store.Active); err != nil {
		t.Fatal(err)
	}
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if resp, _ := f.pull("org/tool", "v1"); resp.StatusCode != http.StatusOK {
		t.Errorf("approved image not tagged: %d", resp.StatusCode)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.gets != before {
		t.Errorf("approval fetched the image again (%d GETs)", u.gets-before)
	}
}

// A reload rebuilds registry clients, so a rotated token file is read.
func TestSyncResetRebuildsClient(t *testing.T) {
	s := &Syncer{}
	r := config.Registry{Name: "r", URL: "https://reg.example", MaxConcurrent: 1}
	a, _ := s.client(r)
	s.Reset()
	b, _ := s.client(r)
	if a == b {
		t.Error("Reset kept the cached client")
	}
}

// A push upstream withdrew by moving the tag back is not applied once it
// is old enough.
func TestSyncMinAgeIgnoresWithdrawnDigest(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	age := 48 * time.Hour
	f.configureSource(u, config.Source{Tags: []string{"latest"}, MinReleaseAge: &age}, config.Registry{})
	a := newImage(MediaOCIManifest, "a", "good")
	b := newImage(MediaOCIManifest, "b", "bad")
	u.add("latest", a)
	poll := func(d time.Duration) []string {
		t.Helper()
		f.now = f.now.Add(d)
		deferred, err := f.syncOnce()
		if err != nil {
			t.Fatal(err)
		}
		return deferred
	}
	poll(0)
	poll(49 * time.Hour) // a applied
	poll(time.Hour)
	u.add("latest", b)
	poll(time.Hour)
	u.add("latest", a) // withdrawn
	var deferred []string
	for range 12 {
		deferred = poll(6 * time.Hour)
	}
	f.publish()
	if _, body := f.pull("org/tool", "latest"); !bytes.Equal(body, a.manifest) {
		t.Error("garner serves the digest upstream withdrew")
	}
	if len(deferred) != 0 {
		t.Errorf("deferred %v while upstream is back on the applied digest", deferred)
	}
}

// Changing as: syncs the images under the new name.
func TestSyncRenamedLocalName(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	u.add("v1", newImage(MediaOCIManifest, "1", "l"))
	f.configureSource(u, config.Source{Tags: []string{"v1"}, As: "old"}, config.Registry{})
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.cfg.Sources[0].As = "new"
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if resp, _ := f.pull("new", "v1"); resp.StatusCode != http.StatusOK {
		t.Errorf("new:v1 after renaming as: %d", resp.StatusCode)
	}
}

// A transient 404 from the CDN on one layer keeps an aged candidate.
func TestSyncTransient404KeepsCandidate(t *testing.T) {
	f := newFixture(t)
	u := newUpstream(t)
	age := 48 * time.Hour
	f.configureSource(u, config.Source{Tags: []string{"latest"}, MinReleaseAge: &age}, config.Registry{})
	b := newImage(MediaOCIManifest, "b", "layer b")
	c := newImage(MediaOCIManifest, "c", "layer c")
	u.add("latest", b)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(30 * time.Hour)
	u.add("latest", c)
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(20 * time.Hour) // b is 50h old, c 20h
	u.mu.Lock()
	u.missing = Digest(b.layers[0])
	u.mu.Unlock()
	if _, err := f.syncOnce(); err == nil {
		t.Fatal("sync with a missing layer succeeded")
	}
	u.mu.Lock()
	u.missing = ""
	u.mu.Unlock()
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	f.publish()
	if _, body := f.pull("org/tool", "latest"); !bytes.Equal(body, b.manifest) {
		t.Error("the aged candidate was dropped after a transient 404")
	}
}

// Syncing a held image leaves its blobs unstaged, also a layer it shares
// with a published image, so pushes can't use them.
func TestSyncHeldBlobsNotStaged(t *testing.T) {
	f := newFixture(t)
	f.push("other", "1", newImage(MediaOCIManifest, "o", "shared"))
	f.publish()
	f.now = f.now.Add(25 * time.Hour) // the push's stages have expired
	if _, err := f.pub.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := newUpstream(t)
	held := newImage(MediaOCIManifest, "h", "shared", "only held")
	u.add("v1", held)
	manual := "manual"
	f.configureSource(u, config.Source{Tags: []string{"v1"}, Approval: &manual}, config.Registry{})
	if _, err := f.syncOnce(); err != nil {
		t.Fatal(err)
	}
	for _, l := range held.layers {
		h := strings.TrimPrefix(Digest(l), "sha256:")
		if staged, _ := f.store.Staged(context.Background(), h, f.now); staged {
			t.Errorf("layer %q of a held image is staged", l)
		}
	}
	if resp, _ := f.do("HEAD", "/v2/x/blobs/"+Digest([]byte("only held")), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD of a held-only layer: %d", resp.StatusCode)
	}
}
