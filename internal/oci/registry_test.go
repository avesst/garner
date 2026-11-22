package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/publish"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

type fixture struct {
	t     *testing.T
	root  string
	cfg   *config.Config
	store *store.Store
	reg   *Registry
	pub   *publish.Publisher
	srv   *httptest.Server
	user  string
	token string
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(root, "garner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	blobs, err := blob.Open(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(root, "key.asc")
	if _, err := signing.GenerateBits(keyFile, "test", "test@example.com", 2048); err != nil {
		t.Fatal(err)
	}
	keys, err := signing.Load([]signing.KeySource{{KeyFile: keyFile}})
	if err != nil {
		t.Fatal(err)
	}
	formats, _ := format.NewRegistry(Format{})
	cfg := config.Default()
	cfg.Deb, cfg.RPM = nil, nil
	cfg.Server.PublicURL = "http://repo.test"
	cfg.Storage.Path = root
	cfg.Storage.GCGrace = 0
	cfg.Hash = "cafe"
	f := &fixture{t: t, root: root, cfg: cfg, now: time.Now()}
	conf := func() *config.Config { return f.cfg }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.store = store.New(d.DB)
	in := &ingest.Ingester{Store: f.store, Blobs: blobs, Formats: formats, Config: conf, TmpDir: root, Log: log}
	f.pub = &publish.Publisher{Store: f.store, Blobs: blobs, Formats: formats, Config: conf,
		Signer: func() signing.Signer { return keys }, Root: root, Log: log}
	f.reg = &Registry{Store: f.store, Blobs: blobs, Ingester: in, Config: conf, Root: root, Log: log,
		Now: func() time.Time { return f.now }}
	f.pub.Collect = append(f.pub.Collect, f.reg.CollectUploads)
	f.pub.Now = func() time.Time { return f.now }
	f.srv = httptest.NewServer(f.reg)
	t.Cleanup(f.srv.Close)
	f.user = "ci"
	f.token = f.newToken("ci", "team", auth.ScopeUpload, auth.ScopeAdmin)
	return f
}

func (f *fixture) newToken(name, owner string, scopes ...string) string {
	secret, _, err := auth.CreateToken(context.Background(), &f.store.Queries, name, owner, scopes)
	if err != nil {
		f.t.Fatal(err)
	}
	return secret
}

// do sends a request as the fixture's token and returns the response
// with its body read.
func (f *fixture) do(method, path string, body []byte, hdr ...string) (*http.Response, []byte) {
	return f.doAs(f.user, f.token, method, path, body, hdr...)
}

func (f *fixture) doAs(user, token, method, path string, body []byte, hdr ...string) (*http.Response, []byte) {
	f.t.Helper()
	u := path
	if !strings.HasPrefix(path, "http") {
		u = f.srv.URL + path
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (f *fixture) want(resp *http.Response, body []byte, status int) {
	f.t.Helper()
	if resp.StatusCode != status {
		f.t.Fatalf("%s %s: status %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, body)
	}
}

// image is a test image: config, layers and its manifest.
type image struct {
	config   []byte
	layers   [][]byte
	manifest []byte
	digest   string
}

func desc(mediaType string, content []byte) Descriptor {
	return Descriptor{MediaType: mediaType, Digest: Digest(content), Size: int64(len(content))}
}

func newImage(mediaType, seed string, layers ...string) *image {
	img := &image{config: []byte(`{"architecture":"amd64","os":"linux","seed":"` + seed + `"}`)}
	m := Manifest{SchemaVersion: 2, MediaType: mediaType,
		Config: ptr(desc("application/vnd.oci.image.config.v1+json", img.config))}
	for _, l := range layers {
		img.layers = append(img.layers, []byte(l))
		m.Layers = append(m.Layers, desc("application/vnd.oci.image.layer.v1.tar", []byte(l)))
	}
	img.manifest, _ = json.Marshal(m)
	img.digest = Digest(img.manifest)
	return img
}

func ptr[T any](v T) *T { return &v }

// pushBlob uploads a blob the way docker does: POST, PATCH, PUT.
func (f *fixture) pushBlob(name string, content []byte) {
	f.t.Helper()
	resp, body := f.do("POST", "/v2/"+name+"/blobs/uploads/", nil)
	f.want(resp, body, http.StatusAccepted)
	loc := resp.Header.Get("Location")
	half := len(content) / 2
	resp, body = f.do("PATCH", loc, content[:half], "Content-Range", fmt.Sprintf("0-%d", max(half-1, 0)))
	f.want(resp, body, http.StatusAccepted)
	resp, body = f.do("PATCH", resp.Header.Get("Location"), content[half:])
	f.want(resp, body, http.StatusAccepted)
	resp, body = f.do("PUT", resp.Header.Get("Location")+"?digest="+Digest(content), nil)
	f.want(resp, body, http.StatusCreated)
}

func (f *fixture) push(name, ref string, img *image) {
	f.t.Helper()
	f.pushBlob(name, img.config)
	for _, l := range img.layers {
		f.pushBlob(name, l)
	}
	f.putManifest(name, ref, img.manifest, http.StatusCreated)
}

func (f *fixture) putManifest(name, ref string, manifest []byte, status int) {
	f.t.Helper()
	var m Manifest
	json.Unmarshal(manifest, &m)
	resp, body := f.do("PUT", "/v2/"+name+"/manifests/"+ref, manifest, "Content-Type", m.MediaType)
	f.want(resp, body, status)
	if status == http.StatusCreated && resp.Header.Get("Docker-Content-Digest") != Digest(manifest) {
		f.t.Fatalf("Docker-Content-Digest %q, want %s", resp.Header.Get("Docker-Content-Digest"), Digest(manifest))
	}
}

func (f *fixture) publish() {
	f.t.Helper()
	if _, err := f.pub.Publish(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) verify() {
	f.t.Helper()
	rep, err := f.pub.Verify(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, p := range rep.Problems {
		if p.Severity == format.SeverityError {
			f.t.Errorf("verify: %s %s: %s", p.Check, p.Path, p.Message)
		}
	}
}

// pull fetches a manifest anonymously, and its blobs.
func (f *fixture) pull(name, ref string) (*http.Response, []byte) {
	f.t.Helper()
	resp, body := f.doAs("", "", "GET", "/v2/"+name+"/manifests/"+ref, nil)
	if resp.StatusCode != http.StatusOK {
		return resp, body
	}
	m, err := ParseManifest(body, resp.Header.Get("Content-Type"))
	if err != nil {
		f.t.Fatalf("pulled manifest: %v", err)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != Digest(body) {
		f.t.Errorf("Docker-Content-Digest %s, body has %s", got, Digest(body))
	}
	for _, d := range m.Manifests {
		if r, b := f.pull(name, d.Digest); r.StatusCode != http.StatusOK {
			f.t.Errorf("child %s: %d %s", d.Digest, r.StatusCode, b)
		}
	}
	for _, h := range m.Blobs() {
		r, b := f.doAs("", "", "GET", "/v2/"+name+"/blobs/sha256:"+h, nil)
		if r.StatusCode != http.StatusOK || Digest(b) != "sha256:"+h {
			f.t.Errorf("blob %s: %d, digest %s", h, r.StatusCode, Digest(b))
		}
	}
	return resp, body
}

func TestPushPublishPull(t *testing.T) {
	f := newFixture(t)
	img := newImage(MediaOCIManifest, "one", "layer one", "layer two")
	f.push("team/tool", "v1", img)

	// Not served before a publish.
	if resp, _ := f.pull("team/tool", "v1"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("pull before publish: %d, want 404", resp.StatusCode)
	}
	f.publish()
	resp, body := f.pull("team/tool", "v1")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, img.manifest) {
		t.Fatalf("pull v1: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != MediaOCIManifest {
		t.Errorf("Content-Type %q", ct)
	}
	if resp, _ := f.pull("team/tool", img.digest); resp.StatusCode != http.StatusOK {
		t.Errorf("pull by digest: %d", resp.StatusCode)
	}
	resp, body = f.doAs("", "", "HEAD", "/v2/team/tool/manifests/v1", nil)
	if resp.StatusCode != http.StatusOK || len(body) != 0 || resp.Header.Get("Docker-Content-Digest") != img.digest {
		t.Errorf("HEAD: %d, %d bytes, digest %q", resp.StatusCode, len(body), resp.Header.Get("Docker-Content-Digest"))
	}
	resp, body = f.doAs("", "", "GET", "/v2/team/tool/tags/list", nil)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != `{"name":"team/tool","tags":["v1"]}` {
		t.Errorf("tags/list: %d %s", resp.StatusCode, body)
	}
	// The static tree serves the same bytes a proxy would.
	static, err := os.ReadFile(filepath.Join(f.root, "public/v2/team/tool/manifests/v1.oci-manifest"))
	if err != nil || !bytes.Equal(static, img.manifest) {
		t.Errorf("static tag file: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(f.root, "public/v2/team/tool/blobs", "sha256:"+img.manifest2hex())); err != nil || !bytes.Equal(b, img.manifest) {
		t.Errorf("static blob link: %v", err)
	}
	page, _ := os.ReadFile(filepath.Join(f.root, "public/index.html"))
	if !strings.Contains(string(page), "<strong>1</strong>images") || strings.Contains(string(page), `class="install"`) ||
		!strings.Contains(string(page), "docker pull repo.test/team/tool:v1") {
		t.Errorf("landing page does not summarize and list the image as expected")
	}
	f.verify()
}

func (img *image) manifest2hex() string { return strings.TrimPrefix(img.digest, "sha256:") }

func TestTagMovesAndOldDigestStays(t *testing.T) {
	f := newFixture(t)
	v1 := newImage(MediaDockerManifest, "one", "a")
	v2 := newImage(MediaDockerManifest, "two", "a", "b")
	f.push("tool", "latest", v1)
	f.publish()
	f.push("tool", "latest", v2)
	f.publish()
	_, body := f.pull("tool", "latest")
	if !bytes.Equal(body, v2.manifest) {
		t.Errorf("latest is not v2")
	}
	if resp, _ := f.pull("tool", v1.digest); resp.StatusCode != http.StatusOK {
		t.Errorf("v1 by digest after the tag moved: %d", resp.StatusCode)
	}
	if ct := f.headType("tool", "latest"); ct != MediaDockerManifest {
		t.Errorf("Content-Type %q", ct)
	}
	f.verify()
}

func (f *fixture) headType(name, ref string) string {
	resp, _ := f.doAs("", "", "HEAD", "/v2/"+name+"/manifests/"+ref, nil)
	return resp.Header.Get("Content-Type")
}

func TestMultiArchIndex(t *testing.T) {
	f := newFixture(t)
	amd := newImage(MediaOCIManifest, "amd64", "shared", "amd only")
	arm := newImage(MediaOCIManifest, "arm64", "shared", "arm only")
	// buildx pushes the platform manifests by digest, then the index.
	for _, img := range []*image{amd, arm} {
		f.push("multi", img.digest, img)
	}
	idx := Manifest{SchemaVersion: 2, MediaType: MediaOCIIndex, Manifests: []Descriptor{
		{MediaType: MediaOCIManifest, Digest: amd.digest, Size: int64(len(amd.manifest)), Platform: &Platform{OS: "linux", Architecture: "amd64"}},
		{MediaType: MediaOCIManifest, Digest: arm.digest, Size: int64(len(arm.manifest)), Platform: &Platform{OS: "linux", Architecture: "arm64"}},
	}}
	body, _ := json.Marshal(idx)
	f.putManifest("multi", "1.0", body, http.StatusCreated)
	f.publish()
	resp, got := f.pull("multi", "1.0")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) || resp.Header.Get("Content-Type") != MediaOCIIndex {
		t.Fatalf("pull index: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// GC keeps every blob below the index, though only the index is a
	// package.
	f.now = f.now.Add(48 * time.Hour)
	if _, err := f.pub.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.pull("multi", "1.0")
	f.verify()
}

func TestIndexWithMissingChildIsRefused(t *testing.T) {
	f := newFixture(t)
	idx := Manifest{SchemaVersion: 2, MediaType: MediaOCIIndex, Manifests: []Descriptor{
		{MediaType: MediaOCIManifest, Digest: Digest([]byte("nope")), Size: 4},
	}}
	body, _ := json.Marshal(idx)
	resp, b := f.do("PUT", "/v2/x/manifests/1", body, "Content-Type", MediaOCIIndex)
	f.want(resp, b, http.StatusBadRequest)
	if !strings.Contains(string(b), codeManifestBlobUnknown) {
		t.Errorf("body %s", b)
	}
}

func TestManifestWithMissingLayerIsRefused(t *testing.T) {
	f := newFixture(t)
	img := newImage(MediaOCIManifest, "x", "never pushed")
	f.pushBlob("x", img.config)
	resp, b := f.do("PUT", "/v2/x/manifests/1", img.manifest, "Content-Type", MediaOCIManifest)
	f.want(resp, b, http.StatusBadRequest)
	if !strings.Contains(string(b), codeManifestBlobUnknown) {
		t.Errorf("body %s", b)
	}
}

func TestPingAndAuth(t *testing.T) {
	f := newFixture(t)
	resp, _ := f.doAs("", "", "GET", "/v2/", nil)
	want := `Bearer realm="http://repo.test/v2/token",service="garner"`
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != want {
		t.Errorf("anonymous ping: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp, _ := f.doAs("ci", "wrong", "GET", "/v2/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token ping: %d", resp.StatusCode)
	}
	if resp, _ := f.do("GET", "/v2/", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("ping: %d", resp.StatusCode)
	}

	// The token endpoint: anonymous tokens only pass the ping; a bad
	// secret gets no token, so docker login fails; a good one's token
	// works like Basic credentials.
	token := func(user, secret string) (int, string) {
		resp, body := f.doAs(user, secret, "GET", "/v2/token?service=garner&scope=repository:x:pull,push", nil)
		var tok struct {
			Token string `json:"token"`
		}
		json.Unmarshal(body, &tok)
		return resp.StatusCode, tok.Token
	}
	bearer := func(tok, method, path string) int {
		resp, _ := f.doAs("", "", method, path, nil, "Authorization", "Bearer "+tok)
		return resp.StatusCode
	}
	status, anon := token("", "")
	if status != http.StatusOK || anon == "" {
		t.Fatalf("anonymous token: %d %q", status, anon)
	}
	if got := bearer(anon, "GET", "/v2/"); got != http.StatusOK {
		t.Errorf("ping with the anonymous token: %d", got)
	}
	if got := bearer(anon, "POST", "/v2/x/blobs/uploads/"); got != http.StatusUnauthorized {
		t.Errorf("push with the anonymous token: %d", got)
	}
	if status, _ := token("ci", "wrong"); status != http.StatusUnauthorized {
		t.Errorf("token for a bad secret: %d", status)
	}
	status, tok := token(f.user, f.token)
	if status != http.StatusOK || tok == anon {
		t.Fatalf("token: %d %q", status, tok)
	}
	if got := bearer(tok, "GET", "/v2/"); got != http.StatusOK {
		t.Errorf("ping with a token: %d", got)
	}
	if got := bearer(tok, "POST", "/v2/x/blobs/uploads/"); got != http.StatusAccepted {
		t.Errorf("push with a token: %d", got)
	}
	if got := bearer("Y2k6d3Jvbmc", "GET", "/v2/"); got != http.StatusUnauthorized { // "ci:wrong"
		t.Errorf("ping with a forged token: %d", got)
	}
	resp, _ = f.doAs("", "", "POST", "/v2/x/blobs/uploads/", nil)
	if want := `,scope="repository:x:pull,push"`; !strings.HasSuffix(resp.Header.Get("WWW-Authenticate"), want) {
		t.Errorf("push challenge %q lacks %s", resp.Header.Get("WWW-Authenticate"), want)
	}
	// Pushing needs the upload scope; deleting needs admin.
	reader := f.newToken("reader", "team", auth.ScopePublish)
	if resp, _ := f.doAs("reader", reader, "POST", "/v2/x/blobs/uploads/", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("push without upload scope: %d", resp.StatusCode)
	}
	if resp, _ := f.doAs("", "", "POST", "/v2/x/blobs/uploads/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous push: %d", resp.StatusCode)
	}
	up := f.newToken("up", "team", auth.ScopeUpload)
	if resp, _ := f.doAs("up", up, "DELETE", "/v2/x/manifests/1", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("delete without admin: %d", resp.StatusCode)
	}
}

func TestOwnershipCheckedBeforeData(t *testing.T) {
	f := newFixture(t)
	f.push("tool", "1", newImage(MediaOCIManifest, "1", "l"))
	other := f.newToken("other", "someone-else", auth.ScopeUpload)
	resp, body := f.doAs("other", other, "POST", "/v2/tool/blobs/uploads/", nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "owned by team") {
		t.Errorf("push to another owner's name: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.doAs("other", other, "POST", "/v2/tool2/blobs/uploads/", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("push to a free name: %d", resp.StatusCode)
	}
}

func TestReservedNameRefused(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do("POST", "/v2/a/blobs/b/blobs/uploads/", nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), codeNameInvalid) {
		t.Errorf("reserved component: %d %s", resp.StatusCode, body)
	}
}

func TestChunkMustStartAtEnd(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do("POST", "/v2/x/blobs/uploads/", nil)
	f.want(resp, body, http.StatusAccepted)
	loc := resp.Header.Get("Location")
	resp, body = f.do("PATCH", loc, []byte("abc"))
	f.want(resp, body, http.StatusAccepted)
	resp, body = f.do("PATCH", loc, []byte("def"), "Content-Range", "0-2")
	f.want(resp, body, http.StatusRequestedRangeNotSatisfiable)
	if resp.Header.Get("Range") != "0-2" {
		t.Errorf("Range %q", resp.Header.Get("Range"))
	}
	resp, body = f.do("GET", loc, nil)
	f.want(resp, body, http.StatusNoContent)
	resp, body = f.do("PUT", loc+"?digest="+Digest([]byte("abcdef")), []byte("def"))
	f.want(resp, body, http.StatusCreated)
}

func TestWrongDigestRefused(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do("POST", "/v2/x/blobs/uploads/?digest="+Digest([]byte("other")), []byte("data"))
	f.want(resp, body, http.StatusBadRequest)
	if !strings.Contains(string(body), codeDigestInvalid) {
		t.Errorf("body %s", body)
	}
}

func TestMountAndHeadDuringPush(t *testing.T) {
	f := newFixture(t)
	f.pushBlob("a", []byte("shared layer"))
	d := Digest([]byte("shared layer"))
	resp, body := f.do("POST", "/v2/b/blobs/uploads/?mount="+d+"&from=a", nil)
	f.want(resp, body, http.StatusCreated)
	// A pushing client checks blobs with HEAD; an unpublished one is
	// found in the blob store.
	resp, _ = f.do("HEAD", "/v2/b/blobs/"+d, nil)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len("shared layer")) {
		t.Errorf("HEAD blob: %d, length %d", resp.StatusCode, resp.ContentLength)
	}
	// Its content is not served before an image that uses it is published.
	if resp, _ := f.doAs("", "", "GET", "/v2/b/blobs/"+d, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET of an unpublished blob: %d", resp.StatusCode)
	}
}

func TestStagedBlobsExpire(t *testing.T) {
	f := newFixture(t)
	f.pushBlob("x", []byte("orphan layer"))
	resp, body := f.do("POST", "/v2/x/blobs/uploads/", nil)
	f.want(resp, body, http.StatusAccepted)
	f.do("PATCH", resp.Header.Get("Location"), []byte("partial"))
	f.publish()
	ctx := context.Background()
	if _, err := f.pub.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.do("HEAD", "/v2/x/blobs/"+Digest([]byte("orphan layer")), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("staged blob collected within upload_expiry: %d", resp.StatusCode)
	}
	f.now = f.now.Add(25 * time.Hour)
	res, err := f.pub.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blobs != 1 {
		t.Errorf("GC removed %d blobs, want the expired staged one", res.Blobs)
	}
	if resp, _ := f.do("HEAD", "/v2/x/blobs/"+Digest([]byte("orphan layer")), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("expired staged blob still served: %d", resp.StatusCode)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, "uploads")); len(entries) != 0 {
		t.Errorf("abandoned upload files left: %d", len(entries))
	}
	if s, _ := f.store.Uploads(ctx); len(s) != 0 {
		t.Errorf("abandoned upload sessions left: %v", s)
	}
}

func TestDeleteTagAndDigest(t *testing.T) {
	f := newFixture(t)
	img := newImage(MediaOCIManifest, "d", "l")
	f.push("tool", "1", img)
	f.push("tool", "stable", img)
	f.publish()
	resp, body := f.do("DELETE", "/v2/tool/manifests/stable", nil)
	f.want(resp, body, http.StatusAccepted)
	f.publish()
	if resp, _ := f.pull("tool", "stable"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted tag still served: %d", resp.StatusCode)
	}
	if resp, _ := f.pull("tool", "1"); resp.StatusCode != http.StatusOK {
		t.Errorf("other tag gone: %d", resp.StatusCode)
	}
	resp, body = f.do("DELETE", "/v2/tool/manifests/"+img.digest, nil)
	f.want(resp, body, http.StatusAccepted)
	f.publish()
	if resp, _ := f.pull("tool", "1"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("tag of a deleted digest still served: %d", resp.StatusCode)
	}
	if resp, _ := f.pull("tool", img.digest); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted digest still served: %d", resp.StatusCode)
	}
	f.verify()
}

func TestRetentionKeepsTaggedAndNewestUntagged(t *testing.T) {
	f := newFixture(t)
	f.cfg.OCI.Retention = 1
	var imgs []*image
	for i := range 4 {
		img := newImage(MediaOCIManifest, fmt.Sprint(i), fmt.Sprint("layer ", i))
		imgs = append(imgs, img)
		f.push("tool", "latest", img)
		if i == 0 {
			f.push("tool", "v0", img)
		}
		f.publish()
	}
	// v0 keeps imgs[0]; latest keeps imgs[3]; of the untagged imgs[1]
	// and imgs[2], only the newest stays.
	for i, want := range []int{200, 404, 200, 200} {
		if resp, _ := f.pull("tool", imgs[i].digest); resp.StatusCode != want {
			t.Errorf("image %d by digest: %d, want %d", i, resp.StatusCode, want)
		}
	}
	f.verify()
}

func TestRollbackRestoresTags(t *testing.T) {
	f := newFixture(t)
	v1 := newImage(MediaOCIManifest, "1", "a")
	v2 := newImage(MediaOCIManifest, "2", "b")
	f.push("tool", "latest", v1)
	s1, err := f.pub.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.push("tool", "latest", v2)
	f.publish()
	if _, err := f.pub.Rollback(context.Background(), s1.ID); err != nil {
		t.Fatal(err)
	}
	if _, body := f.pull("tool", "latest"); !bytes.Equal(body, v1.manifest) {
		t.Errorf("latest after rollback is not v1")
	}
	// The next publish keeps the rolled-back tag.
	f.publish()
	if _, body := f.pull("tool", "latest"); !bytes.Equal(body, v1.manifest) {
		t.Errorf("latest after the next publish is not v1")
	}
	f.verify()
}

func TestTagPagination(t *testing.T) {
	f := newFixture(t)
	img := newImage(MediaOCIManifest, "p", "l")
	for _, tag := range []string{"a", "b", "c"} {
		f.push("tool", tag, img)
	}
	f.publish()
	resp, body := f.doAs("", "", "GET", "/v2/tool/tags/list?n=2", nil)
	if strings.TrimSpace(string(body)) != `{"name":"tool","tags":["a","b"]}` || !strings.Contains(resp.Header.Get("Link"), "last=b") {
		t.Errorf("first page: %s, Link %q", body, resp.Header.Get("Link"))
	}
	_, body = f.doAs("", "", "GET", "/v2/tool/tags/list?n=2&last=b", nil)
	if strings.TrimSpace(string(body)) != `{"name":"tool","tags":["c"]}` {
		t.Errorf("second page: %s", body)
	}
}

func TestVerifyFindsTamperedTree(t *testing.T) {
	f := newFixture(t)
	img := newImage(MediaOCIManifest, "v", "l")
	f.push("tool", "1", img)
	f.publish()
	f.verify()
	tagFile := filepath.Join(f.root, "public/v2/tool/manifests/1.oci-manifest")
	// Snapshot files are read-only for others, not for the owner.
	if err := os.WriteFile(tagFile, []byte(`{"schemaVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(f.root, "public/v2/tool/manifests/stray.oci-manifest")
	os.WriteFile(stray, img.manifest, 0o644)
	rep, err := f.pub.Verify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, p := range rep.Problems {
		if p.Severity == format.SeverityError {
			found = append(found, p.Check+" "+p.Message)
		}
	}
	joined := strings.Join(found, "\n")
	if !strings.Contains(joined, "has digest") || !strings.Contains(joined, "no such tag") {
		t.Errorf("verify problems:\n%s", joined)
	}
}

// A blob stored for something else, such as a held package, is not
// reachable by mounting or referencing its digest.
func TestForeignBlobNotUsable(t *testing.T) {
	f := newFixture(t)
	secret := []byte("content of a held package")
	b, err := f.reg.Blobs.Put(bytes.NewReader(secret))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.AddBlob(context.Background(), b.SHA256, b.Size); err != nil {
		t.Fatal(err)
	}
	d := "sha256:" + b.SHA256
	resp, body := f.do("POST", "/v2/x/blobs/uploads/?mount="+d+"&from=y", nil)
	f.want(resp, body, http.StatusAccepted) // a new session, not a mount
	if resp, _ := f.do("HEAD", "/v2/x/blobs/"+d, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD of a foreign blob: %d", resp.StatusCode)
	}
	img := &image{config: secret}
	m := Manifest{SchemaVersion: 2, MediaType: MediaOCIManifest, Config: ptr(desc("application/vnd.oci.image.config.v1+json", secret))}
	img.manifest, _ = json.Marshal(m)
	resp, body = f.do("PUT", "/v2/x/manifests/1", img.manifest, "Content-Type", MediaOCIManifest)
	f.want(resp, body, http.StatusBadRequest)
}

// A chunk still arriving while the session is finished can't change the
// stored blob.
func TestConcurrentChunkCannotCorruptBlob(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do("POST", "/v2/x/blobs/uploads/", nil)
	f.want(resp, body, http.StatusAccepted)
	loc := resp.Header.Get("Location")
	resp, body = f.do("PATCH", loc, []byte("AAAA"))
	f.want(resp, body, http.StatusAccepted)

	pr, pw := io.Pipe()
	req, _ := http.NewRequest("PATCH", f.srv.URL+loc, pr)
	req.SetBasicAuth(f.user, f.token)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	pw.Write([]byte("BBBB"))
	time.Sleep(100 * time.Millisecond)
	resp, body = f.do("PUT", loc+"?digest="+Digest([]byte("AAAA")), nil)
	f.want(resp, body, http.StatusCreated)
	pw.Write([]byte("CCCC"))
	pw.Close()
	<-done

	got, err := os.ReadFile(f.reg.Blobs.PathOf(strings.TrimPrefix(Digest([]byte("AAAA")), "sha256:")))
	if err != nil || string(got) != "AAAA" {
		t.Errorf("stored blob %q, %v", got, err)
	}
}

// A HEAD that lets a client skip a staged layer keeps it for the push.
func TestStagedHeadExtendsStage(t *testing.T) {
	f := newFixture(t)
	f.pushBlob("x", []byte("layer"))
	d := Digest([]byte("layer"))
	f.now = f.now.Add(23 * time.Hour)
	if resp, _ := f.do("HEAD", "/v2/x/blobs/"+d, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: %d", resp.StatusCode)
	}
	f.now = f.now.Add(2 * time.Hour)
	if _, err := f.pub.GC(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.do("HEAD", "/v2/x/blobs/"+d, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("layer collected after a HEAD 2h ago: %d", resp.StatusCode)
	}
}

// A session whose file is still being written is not abandoned, though its
// row has not changed for longer than upload_expiry.
func TestUploadInProgressNotCollected(t *testing.T) {
	f := newFixture(t)
	resp, body := f.do("POST", "/v2/x/blobs/uploads/", nil)
	f.want(resp, body, http.StatusAccepted)
	id := resp.Header.Get("Docker-Upload-UUID")
	f.now = f.now.Add(25 * time.Hour)
	if err := os.Chtimes(f.reg.uploadPath(id), f.now, f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.CollectUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.GetUpload(context.Background(), id); err != nil {
		t.Errorf("active session collected: %v", err)
	}
}
