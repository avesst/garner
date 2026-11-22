package publish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/oci"
	"github.com/avesst/garner/internal/publish"
	"github.com/avesst/garner/internal/rpm"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

// landing is a repository with the given formats, for landing page tests.
type landing struct {
	t    *testing.T
	root string
	cfg  *config.Config
	in   *ingest.Ingester
	pub  *publish.Publisher
}

func newLanding(t *testing.T, cfg *config.Config, fms func(signer func() signing.Signer) []format.Format) *landing {
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
	signer := func() signing.Signer { return keys }
	reg, err := format.NewRegistry(fms(signer)...)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.PublicURL = "https://repo.test"
	cfg.Storage.Path = root
	cfg.Hash = "cafe"
	conf := func() *config.Config { return cfg }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(d.DB)
	l := &landing{t: t, root: root, cfg: cfg}
	l.in = &ingest.Ingester{Store: st, Blobs: blobs, Formats: reg, Config: conf, TmpDir: root, Log: log}
	l.pub = &publish.Publisher{Store: st, Blobs: blobs, Formats: reg, Config: conf, Signer: signer, Root: root, Log: log}
	return l
}

func (l *landing) upload(data []byte) {
	l.t.Helper()
	if _, err := l.in.Ingest(context.Background(), bytes.NewReader(data),
		ingest.Request{Owner: "team", Origin: "upload", OriginRef: "t", VerifyStatus: "n/a"}); err != nil {
		l.t.Fatal(err)
	}
}

// image records an image with the given platforms and tags, as a push
// would.
func (l *landing) image(name string, platforms []string, tags ...string) {
	l.t.Helper()
	ctx := context.Background()
	cfgBlob := []byte(`{"architecture":"amd64","os":"linux","name":"` + name + `"}`)
	b, err := l.in.Blobs.Put(bytes.NewReader(cfgBlob))
	if err != nil {
		l.t.Fatal(err)
	}
	if err := l.in.Store.AddBlob(ctx, b.SHA256, b.Size); err != nil {
		l.t.Fatal(err)
	}
	manifest, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaOCIManifest,
		Config: &oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: "sha256:" + b.SHA256, Size: b.Size}})
	md, _ := json.Marshal(oci.Metadata{MediaType: oci.MediaOCIManifest, Size: int64(len(manifest)), Platforms: platforms})
	if _, err := l.in.Record(ctx, ingest.Delivery{
		Format: oci.Format{}, Content: manifest, Blobs: []string{b.SHA256}, Tags: tags,
		Parsed:  &format.Parsed{Name: name, Version: oci.Digest(manifest), Metadata: md},
		Request: ingest.Request{Owner: "team", Origin: "upload", OriginRef: "t", VerifyStatus: "n/a"},
	}); err != nil {
		l.t.Fatal(err)
	}
}

func (l *landing) publish() {
	l.t.Helper()
	if _, err := l.pub.Publish(context.Background()); err != nil {
		l.t.Fatal(err)
	}
}

func (l *landing) read(name string) string {
	l.t.Helper()
	b, err := os.ReadFile(filepath.Join(l.root, "public", name))
	if err != nil {
		l.t.Fatal(err)
	}
	return string(b)
}

// TestLandingByEcosystem renders deb, rpm and images: one tab per
// namespace, the key only with the signed one, and a detail page each.
// With GARNER_LANDING_SAMPLE set to a directory, the pages are copied
// there for a look.
func TestLandingByEcosystem(t *testing.T) {
	for _, tool := range []string{"createrepo_c", "rpmsign", "gpg", "gpgconf"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	cfg := config.Default()
	cfg.Deb.Suites = []string{"stable", "testing"}
	l := newLanding(t, cfg, func(signer func() signing.Signer) []format.Format {
		r, err := rpm.NewFormat(signer)
		if err != nil {
			t.Fatal(err)
		}
		return []format.Format{deb.Format{}, oci.Format{}, r}
	})
	l.upload(debtest.Build("app", "1.0", "amd64", "app 1.0"))
	l.upload(debtest.Build("app", "1.1", "amd64", "app 1.1"))
	l.upload(debtest.Build("tool", "2.0", "all", "tool"))
	for _, f := range []string{"garner-hello-1.0-1.noarch.rpm", "garner-hello-2.0-1.x86_64.rpm"} {
		data, err := os.ReadFile(filepath.Join("..", "rpm", "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		l.upload(data)
	}
	l.image("team/api", []string{"linux/amd64", "linux/arm64"}, "1.0", "latest")
	l.image("team/worker", nil, "2.3")
	l.image("mirror/alpine", []string{"linux/amd64"}, "3.22")
	l.publish()

	page := l.read("index.html")
	linux := strings.Index(page, `id="eco-os"`)
	images := strings.Index(page, `id="eco-oci"`)
	key := strings.Index(page, "<h2>Signing key</h2>")
	if linux < 0 || images < 0 || key < 0 || !(linux < key && key < images) || strings.Count(page, "<h2>Signing key</h2>") != 1 {
		t.Errorf("tabs or key misplaced: linux %d, images %d, key %d", linux, images, key)
	}
	for _, want := range []string{
		`<h2 class="eco-h">Linux packages</h2>`, `<h2 class="eco-h">Container images</h2>`,
		`<h2>Browse <span class="muted count">(3)</span></h2>`,
		`<div class="src" data-kind="forge">`, `<div class="src" data-kind="registry">`,
		`<small>apt</small>`, `<small>dnf</small>`, `data-dist="testing"`,
		"docker pull repo.test/&lt;name&gt;:&lt;tag&gt;", "<code>team/</code> 2", "<code>mirror/</code> 1",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	if n := strings.Count(page, `<h2>Browse <span class="muted count">(3)</span></h2>`); n != 2 {
		t.Errorf("%d tabs have the Browse list, want 2", n)
	}
	// Each tab lists its own names: packages in the Linux tab, images in
	// the images tab.
	pkgs, imgs := page[linux:images], page[images:]
	for _, want := range []string{
		`data-name="app"`, "<dt>Debian / Ubuntu · stable</dt><dd>1.1 (amd64), 1.0 (amd64)</dd>", "<dt>Debian / Ubuntu · stable</dt><dd>2.0 (all)</dd>",
		`data-name="garner-hello"`, "<dt>Fedora / RHEL · stable</dt><dd>3:2.0-1 (x86_64), 1.0-1 (noarch)</dd>",
	} {
		if !strings.Contains(pkgs, want) {
			t.Errorf("the Linux tab lacks %q", want)
		}
	}
	if strings.Contains(pkgs, "team/api") || strings.Contains(imgs, `data-name="app"`) {
		t.Error("a tab lists the other ecosystem's names")
	}
	for _, want := range []string{
		`data-name="team/api"`, `data-name="mirror/alpine"`,
		`data-copy="docker pull repo.test/team/api:latest"`, "<dd>linux/amd64, linux/arm64</dd>",
		`data-copy="docker pull repo.test/team/worker:2.3"`,
	} {
		if !strings.Contains(imgs, want) {
			t.Errorf("the images tab lacks %q", want)
		}
	}
	if strings.Contains(imgs, ">docker pull repo.test/team/api") {
		t.Error("the pull command is printed; only its copy button should carry it")
	}
	if strings.Index(imgs, `data-name="mirror/alpine"`) > strings.Index(imgs, `data-name="team/api"`) {
		t.Error("images are not ordered by namespace")
	}
	if strings.Contains(page, "ZgotmplZ") {
		t.Error("the page has a value the template escaper rejected")
	}
	for _, gone := range []string{"packages.html", "images.html"} {
		if _, err := os.Stat(filepath.Join(l.root, "public", gone)); err == nil {
			t.Errorf("%s is still written", gone)
		}
	}

	if dir := getenv("GARNER_LANDING_SAMPLE"); dir != "" {
		for _, name := range []string{"index.html"} {
			if err := writeFile(filepath.Join(dir, name), l.read(name)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// With 500 images, the summary stays short and the tab's list holds them
// all; the page's script shows them a page at a time.
func TestLandingManyImages(t *testing.T) {
	cfg := config.Default()
	cfg.Deb, cfg.RPM = nil, nil
	l := newLanding(t, cfg, func(func() signing.Signer) []format.Format { return []format.Format{oci.Format{}} })
	for i := range 500 {
		l.image(fmt.Sprintf("ns%02d/image%03d", i%30, i), []string{"linux/amd64"}, "1.0")
	}
	l.image("ns00/many", nil, "a", "b", "c", "d", "e", "f", "g", "h")
	l.publish()
	page := l.read("index.html")
	if !strings.Contains(page, "<strong>501</strong>images") || !strings.Contains(page, "and 18 more") {
		t.Errorf("summary lacks the image count or the capped namespaces")
	}
	if strings.Contains(page, `class="eco-tabs"`) || strings.Contains(page, "Signing key") {
		t.Error("an image-only repository shows tabs or the signing key")
	}
	if n := strings.Count(page, `<li class="row"`); n != 501 {
		t.Errorf("the page lists %d images, want 501", n)
	}
	if !strings.Contains(page, `href="https://repo.test/v2/ns00/many/tags/list">+3 more</a>`) {
		t.Error("the list does not cap the tags of ns00/many at 5 with a link to the rest")
	}
	if dir := getenv("GARNER_LANDING_SAMPLE"); dir != "" {
		if err := writeFile(filepath.Join(dir, "index-many.html"), page); err != nil {
			t.Fatal(err)
		}
	}
}

func getenv(k string) string { return os.Getenv(k) }

func writeFile(path, s string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s), 0o644)
}
