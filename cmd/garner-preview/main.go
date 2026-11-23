// Command garner-preview serves the landing page with demo packages, for
// working on the page. It is a development tool and not part of a release.
//
// It builds a throwaway repository in a temporary directory, publishes it,
// and serves it with a made-up /api/v1/status. landing.html is read from
// disk: an edit republishes and reloads open browser tabs. A restart (for
// changes to Go code) also reloads them once the preview is back.
//
//	go run ./cmd/garner-preview            # from the repository root
//	go run ./cmd/garner-preview -sources 12 -failing 3 -extra 60
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

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
	"github.com/avesst/garner/internal/server"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

func main() {
	listen := flag.String("listen", "localhost:8090", "address to serve on")
	dir := flag.String("templates", "internal/publish", "directory holding landing.html")
	testdata := flag.String("rpm-testdata", "internal/rpm/testdata", "directory holding the demo .rpm files")
	sources := flag.Int("sources", 4, "sync sources in the status, alternately forge and registry")
	failing := flag.Int("failing", 1, "how many of the sources report an error")
	conflicts := flag.Int("conflicts", 1, "open conflicts in the status")
	down := flag.Bool("down", false, "answer /api/v1/status with 503, as if garner were unreachable")
	extra := flag.Int("extra", 30, "extra deb packages and images, to fill the paged lists")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *listen, *dir, *testdata, *extra, status{*sources, *failing, *conflicts, *down}); err != nil {
		log.Error("preview failed", "err", err)
		os.Exit(1)
	}
}

type status struct {
	sources, failing, conflicts int
	down                        bool
}

func run(log *slog.Logger, listen, dir, testdata string, extra int, st status) error {
	if _, err := os.Stat(filepath.Join(dir, "landing.html")); err != nil {
		return fmt.Errorf("%w (run from the repository root, or pass -templates)", err)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "garner-preview-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := newRepo(ctx, log, root, "http://"+ln.Addr().String(), dir)
	if err != nil {
		return err
	}
	defer r.db.Close()
	if err := r.fill(ctx, testdata, extra); err != nil {
		return err
	}
	r.republish(ctx)

	go r.watch(ctx, filepath.Join(dir, "landing.html"))
	srv := &http.Server{Handler: r.handler(st)}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Info("preview ready", "url", "http://"+ln.Addr().String()+"/", "storage", root)
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

type repo struct {
	log  *slog.Logger
	root string
	db   *db.DB
	in   *ingest.Ingester
	pub  *publish.Publisher
	rpm  bool

	mu       sync.Mutex
	snapshot string
	err      error // of the last publish; shown in place of the page
	version  int   // counts publishes, for the reload events
	changed  chan struct{}
}

func newRepo(ctx context.Context, log *slog.Logger, root, url, dir string) (*repo, error) {
	d, err := db.Open(ctx, filepath.Join(root, "garner.db"))
	if err != nil {
		return nil, err
	}
	blobs, err := blob.Open(filepath.Join(root, "blobs"))
	if err != nil {
		return nil, err
	}
	keyFile := filepath.Join(root, "key.asc")
	if _, err := signing.GenerateBits(keyFile, "garner preview", "preview@example.com", 2048); err != nil {
		return nil, err
	}
	keys, err := signing.Load([]signing.KeySource{{KeyFile: keyFile}})
	if err != nil {
		return nil, err
	}
	signer := func() signing.Signer { return keys }

	cfg := config.Default()
	cfg.Server.PublicURL = url
	cfg.Storage.Path = root
	cfg.Hash = "preview"
	cfg.Deb.Suites = []string{"stable", "testing"}
	cfg.Deb.Architectures = []string{"amd64", "arm64"}
	cfg.RPM.Releases = []string{"stable", "testing"}
	cfg.RPM.Architectures = []string{"x86_64", "aarch64"}
	fms := []format.Format{deb.Format{}}
	hasRPM := true
	if f, err := rpm.NewFormat(signer); err != nil {
		log.Warn("previewing without rpm", "err", err)
		cfg.RPM, hasRPM = nil, false
	} else {
		fms = append(fms, f)
	}
	fms = append(fms, oci.Format{})
	reg, err := format.NewRegistry(fms...)
	if err != nil {
		return nil, err
	}
	conf := func() *config.Config { return cfg }
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(d.DB)
	return &repo{
		log: log, root: root, db: d, rpm: hasRPM, changed: make(chan struct{}),
		in: &ingest.Ingester{Store: st, Blobs: blobs, Formats: reg, Config: conf, TmpDir: root, Log: quiet},
		pub: &publish.Publisher{Store: st, Blobs: blobs, Formats: reg, Config: conf, Signer: signer,
			Root: root, Log: quiet, LandingFS: os.DirFS(dir)},
	}, nil
}

var upload = ingest.Request{Owner: "team", Origin: "upload", OriginRef: "preview", VerifyStatus: "n/a"}

// fill adds the demo packages. The fixed ones cover the grid's empty
// cells, several versions and both suites; extra ones fill the paged
// lists.
func (r *repo) fill(ctx context.Context, testdata string, extra int) error {
	type d struct{ name, version, arch, suite string }
	debs := []d{
		{"app", "1.0", "amd64", ""}, {"app", "1.1", "amd64", ""}, {"app", "1.1", "arm64", ""},
		{"tool", "2.0", "all", ""}, {"agent", "0.9~rc1", "amd64", "testing"},
	}
	for i := range extra {
		debs = append(debs, d{fmt.Sprintf("lib%02d", i), "1." + fmt.Sprint(i%4), "amd64", ""})
	}
	for _, p := range debs {
		req := upload
		if p.suite != "" {
			req.Targets = map[string]string{"suite": p.suite}
		}
		data := debtest.Build(p.name, p.version, p.arch, p.name+" "+p.version)
		if _, err := r.in.Ingest(ctx, bytes.NewReader(data), req); err != nil {
			return fmt.Errorf("deb %s: %w", p.name, err)
		}
	}
	if r.rpm {
		for _, f := range []string{"garner-hello-1.0-1.noarch.rpm", "garner-hello-2.0-1.x86_64.rpm"} {
			data, err := os.ReadFile(filepath.Join(testdata, f))
			if err != nil {
				return err
			}
			if _, err := r.in.Ingest(ctx, bytes.NewReader(data), upload); err != nil {
				return fmt.Errorf("rpm %s: %w", f, err)
			}
		}
	}
	images := []struct {
		name      string
		platforms []string
		tags      []string
	}{
		{"team/api", []string{"linux/amd64", "linux/arm64"}, []string{"1.0", "latest"}},
		{"team/worker", nil, []string{"2.3"}},
		{"mirror/alpine", []string{"linux/amd64"}, []string{"3.22"}},
	}
	for _, im := range images {
		if err := r.image(ctx, im.name, im.platforms, im.tags...); err != nil {
			return err
		}
	}
	for i := range extra {
		if err := r.image(ctx, fmt.Sprintf("ns%d/image%02d", i%5, i), []string{"linux/amd64"}, "1.0"); err != nil {
			return err
		}
	}
	return nil
}

// image records an image as a push would, with an empty config blob as
// its only content.
func (r *repo) image(ctx context.Context, name string, platforms []string, tags ...string) error {
	cfgBlob := []byte(`{"architecture":"amd64","os":"linux","name":"` + name + `"}`)
	b, err := r.in.Blobs.Put(bytes.NewReader(cfgBlob))
	if err != nil {
		return err
	}
	if err := r.in.Store.AddBlob(ctx, b.SHA256, b.Size); err != nil {
		return err
	}
	manifest, err := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaOCIManifest,
		Config: &oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: "sha256:" + b.SHA256, Size: b.Size}})
	if err != nil {
		return err
	}
	md, err := json.Marshal(oci.Metadata{MediaType: oci.MediaOCIManifest, Size: int64(len(manifest)), Platforms: platforms})
	if err != nil {
		return err
	}
	_, err = r.in.Record(ctx, ingest.Delivery{
		Format: oci.Format{}, Content: manifest, Blobs: []string{b.SHA256}, Tags: tags,
		Parsed:  &format.Parsed{Name: name, Version: oci.Digest(manifest), Metadata: md},
		Request: upload,
	})
	if err != nil {
		return fmt.Errorf("image %s: %w", name, err)
	}
	return nil
}

// republish renders a new snapshot and tells open pages to reload.
func (r *repo) republish(ctx context.Context) {
	snap, err := r.pub.Publish(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
	if err != nil {
		r.log.Error("publish failed", "err", err)
	} else {
		r.snapshot = snap.ID
		r.log.Info("published", "snapshot", snap.ID)
	}
	r.version++
	close(r.changed)
	r.changed = make(chan struct{})
}

// watch republishes when the template's modification time changes. It
// polls, which needs no file notification module and is cheap for one
// file.
func (r *repo) watch(ctx context.Context, file string) {
	last := modTime(file)
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if m := modTime(file); !m.Equal(last) {
			last = m
			r.republish(ctx)
		}
	}
}

func modTime(file string) time.Time {
	fi, err := os.Stat(file)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func (r *repo) handler(st status) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		if st.down {
			http.Error(w, "preview: status is down (-down)", http.StatusServiceUnavailable)
			return
		}
		r.mu.Lock()
		snap := r.snapshot
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(demoStatus(st, snap))
	})
	mux.HandleFunc("GET /_preview/events", r.events)
	files := http.FileServer(http.Dir(filepath.Join(r.root, "public")))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.mu.Lock()
		perr := r.err
		r.mu.Unlock()
		p := req.URL.Path
		if strings.HasSuffix(p, "/") {
			p += "index.html"
		}
		if path.Ext(p) != ".html" {
			files.ServeHTTP(w, req)
			return
		}
		if perr != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "<!doctype html><title>Publish failed</title><h1>Publish failed</h1><pre>%s</pre>%s",
				htmlEscape(perr.Error()), reloadScript)
			return
		}
		page, err := os.ReadFile(filepath.Join(r.root, "public", filepath.FromSlash(path.Clean(p))))
		if err != nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if i := bytes.LastIndex(page, []byte("</body>")); i >= 0 {
			page = append(page[:i:i], append([]byte(reloadScript), page[i:]...)...)
		}
		w.Write(page)
	})
	return mux
}

// events streams one event per publish. The page reloads on each, and
// also when the stream comes back after the preview restarted.
func (r *repo) events(w http.ResponseWriter, req *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	for {
		r.mu.Lock()
		ch, v := r.changed, r.version
		r.mu.Unlock()
		select {
		case <-req.Context().Done():
			return
		case <-ch:
			fmt.Fprintf(w, "data: %d\n\n", v+1)
			fl.Flush()
		}
	}
}

const reloadScript = `<script>
(function () {
  var down = false, es = new EventSource("/_preview/events");
  es.onmessage = function () { location.reload(); };
  es.onerror = function () { down = true; };
  es.onopen = function () { if (down) location.reload(); };
})();
</script>
`

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// demoStatus is a status with made-up sources: the first ones failing,
// every other one a registry.
func demoStatus(st status, snapshot string) server.Status {
	now := time.Now().UTC()
	out := server.Status{Version: "preview", ConfigHash: "preview", CurrentSnapshot: snapshot,
		PublishedAt: now.Format(time.RFC3339), Sources: []server.SourceStatus{}, OpenConflicts: st.conflicts}
	for i := range st.sources {
		s := server.SourceStatus{Name: fmt.Sprintf("forge-%02d", i), Kind: "forge"}
		if i%2 == 1 {
			s.Name, s.Kind = fmt.Sprintf("registry-%02d", i), "registry"
		}
		run := now.Add(-time.Duration(i+1) * 7 * time.Minute)
		s.LastRun = run.Format(time.RFC3339)
		if i < st.failing {
			s.LastError = "GET https://example.com/releases: 502 Bad Gateway"
			s.LastSuccess = run.Add(-26 * time.Hour).Format(time.RFC3339)
		} else {
			s.LastSuccess = s.LastRun
		}
		out.Sources = append(out.Sources, s)
	}
	return out
}
