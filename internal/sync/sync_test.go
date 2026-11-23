package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/forge"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/server"
	"github.com/avesst/garner/internal/store"
)

// fakeForge serves releases and asset bodies from memory.
type fakeForge struct {
	repos    []forge.Repo
	releases map[string][]forge.Release
	files    map[string][]byte // asset URL -> body
	fail     map[string]bool   // asset URL -> download fails
	etag     string            // when set, a request with it is not modified
	// wholePage lists every release, as a real forge's first page also
	// holds releases older than since.
	wholePage bool
	listFail  map[string]bool   // repo -> Releases fails
	unlisted  map[string]bool   // release ID -> left out of listings, found by ID
	renamed   map[string]string // old release ID -> the ID the release has now
	lookFail  map[string]bool   // release ID -> lookup by ID fails

	mu        gosync.Mutex
	calls     map[string]int           // repo -> Releases calls
	downloads []string                 // asset names, in order
	block     map[string]chan struct{} // repo -> Releases waits for close
}

func (f *fakeForge) count(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[repo]
}

func (f *fakeForge) Name() string            { return "fake" }
func (f *fakeForge) RateLimitRemaining() int { return -1 }
func (f *fakeForge) ListRepos(context.Context, string, bool) ([]forge.Repo, error) {
	return f.repos, nil
}

// Releases returns only what the interface promises: the releases
// published at or after since, so a run that asks for too short a listing
// misses older ones as it would on a real forge.
func (f *fakeForge) Releases(ctx context.Context, repo string, since time.Time, etag string) ([]forge.Release, string, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[repo]++
	block := f.block[repo]
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	if f.listFail[repo] {
		return nil, "", errors.New("HTTP 502")
	}
	if f.etag != "" && etag == f.etag {
		return nil, "", forge.ErrNotModified
	}
	var out []forge.Release
	for _, r := range f.releases[repo] {
		// A real listing reaches only releases at or after since, but
		// cannot place one without a publish time, so it keeps those.
		if f.unlisted[r.ID] {
			continue
		}
		if f.wholePage || r.PublishedAt.IsZero() || !r.PublishedAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, f.etag, nil
}
func (f *fakeForge) Release(_ context.Context, repo, id string) (forge.Release, bool, error) {
	if f.listFail[repo] || f.lookFail[id] {
		return forge.Release{}, false, errors.New("HTTP 502")
	}
	if now, ok := f.renamed[id]; ok {
		id = now
	}
	for _, r := range f.releases[repo] {
		if r.ID == id {
			return r, true, nil
		}
	}
	return forge.Release{}, false, nil
}

func (f *fakeForge) Download(_ context.Context, a forge.Asset) (io.ReadCloser, error) {
	f.mu.Lock()
	f.downloads = append(f.downloads, a.Name)
	f.mu.Unlock()
	if f.fail[a.URL] {
		return nil, errors.New("connection reset")
	}
	b, ok := f.files[a.URL]
	if !ok {
		return nil, &forge.StatusError{URL: a.URL, Code: 404}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// add puts an asset with the given body into a release.
func (f *fakeForge) add(rel *forge.Release, name string, body []byte) forge.Asset {
	a := forge.Asset{Name: name, URL: "mem://" + rel.ID + "/" + name, Size: int64(len(body))}
	f.files[a.URL] = body
	rel.Assets = append(rel.Assets, a)
	return a
}

type fixture struct {
	t     *testing.T
	cfg   *config.Config
	live  atomic.Pointer[config.Config] // what the manager reads
	st    *store.Store
	forge *fakeForge
	m     *Manager
}

func newFixture(t *testing.T, src config.Source) *fixture {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "garner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	blobs, _ := blob.Open(filepath.Join(dir, "blobs"))
	reg, _ := format.NewRegistry(deb.Format{})
	cfg := config.Default()
	cfg.Deb.Suites = []string{"stable", "noble"}
	cfg.Forges = []config.Forge{{Name: "gh", Type: "github", MaxConcurrent: 1}}
	src.Forge = "gh"
	cfg.Sources = []config.Source{src}
	f := &fixture{t: t, cfg: cfg, forge: &fakeForge{releases: map[string][]forge.Release{},
		files: map[string][]byte{}, fail: map[string]bool{}}}
	f.st = store.New(d.DB)
	f.live.Store(cfg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	conf := func() *config.Config { return f.live.Load() }
	f.m = &Manager{
		Store: f.st, Formats: reg, Config: conf, TmpDir: dir, Log: log,
		Ingester:  &ingest.Ingester{Store: f.st, Blobs: blobs, Formats: reg, Config: conf, TmpDir: dir, Log: log},
		NewClient: func(config.Forge) (forge.Client, error) { return f.forge, nil },
	}
	// Start creates the clients; the poll loops are stopped at once so
	// they cannot race the test's own RunOnce calls.
	if err := f.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.m.Stop()
	return f
}

func (f *fixture) release(repo, id string, day int) *forge.Release {
	rel := forge.Release{ID: id, Tag: "v" + id, PublishedAt: time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC)}
	f.forge.releases[repo] = append([]forge.Release{rel}, f.forge.releases[repo]...)
	return &f.forge.releases[repo][0]
}

func (f *fixture) sync() *store.SourceState {
	f.t.Helper()
	f.m.RunOnce(context.Background(), f.cfg.Sources[0])
	st, err := f.st.SourceState(context.Background(), f.cfg.Sources[0].Name)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

// placed returns "name version dist state verify" for every placement.
func (f *fixture) placed() []string {
	f.t.Helper()
	pkgs, err := f.st.Placed(context.Background(), store.PackageFilter{})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, p := range pkgs {
		out = append(out, fmt.Sprintf("%s %s %s %s %s", p.Name, p.Version, p.Distribution, p.State, p.VerifyStatus))
	}
	return out
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestFirstRunTakesLatestThenNewer(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 2)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	draft := f.release("org/tool", "3", 3)
	draft.Draft = true
	f.forge.add(draft, "tool_3.0_amd64.deb", debtest.Build("tool", "3.0", "amd64", "3"))

	if st := f.sync(); st.LastError != "" || st.LastSuccess == "" {
		t.Fatalf("state = %+v", st)
	}
	if got := strings.Join(f.placed(), "; "); got != "tool 2.0 stable active unverified" {
		t.Fatalf("first run placed %q, want only the latest release", got)
	}

	pre := f.release("org/tool", "4", 4)
	pre.Prerelease = true
	f.forge.add(pre, "tool_4.0~rc1_amd64.deb", debtest.Build("tool", "4.0~rc1", "amd64", "4"))
	r5 := f.release("org/tool", "5", 5)
	f.forge.add(r5, "tool_5.0_amd64.deb", debtest.Build("tool", "5.0", "amd64", "5"))
	r6 := f.release("org/tool", "6", 6)
	f.forge.add(r6, "tool_6.0_amd64.deb", debtest.Build("tool", "6.0", "amd64", "6"))
	f.sync()
	got := strings.Join(f.placed(), "; ")
	for _, want := range []string{"tool 5.0", "tool 6.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("later run lacks %s: %s", want, got)
		}
	}
	if strings.Contains(got, "4.0~rc1") || strings.Contains(got, "3.0") || strings.Contains(got, "1.0") {
		t.Errorf("later run took a prerelease, a draft or an old release: %s", got)
	}
}

func TestChecksumsAndVerify(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	good := debtest.Build("tool", "1.0", "amd64", "good")
	bad := debtest.Build("tool-extra", "1.0", "amd64", "bad")
	digest := debtest.Build("tool-digest", "1.0", "amd64", "d")
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool_1.0_amd64.deb", good)
	f.forge.add(rel, "tool-extra_1.0_amd64.deb", bad)
	a := f.forge.add(rel, "tool-digest_1.0_amd64.deb", digest)
	rel.Assets[len(rel.Assets)-1].SHA256 = sum(digest)
	_ = a
	f.forge.add(rel, "checksums.txt", []byte(fmt.Sprintf("%s  tool_1.0_amd64.deb\n%s *tool-extra_1.0_amd64.deb\n",
		sum(good), strings.Repeat("0", 64))))

	st := f.sync()
	if !strings.Contains(st.LastError, "checksum mismatch") {
		t.Errorf("LastError = %q, want a checksum mismatch", st.LastError)
	}
	got := strings.Join(f.placed(), "; ")
	if !strings.Contains(got, "tool 1.0 stable active verified") || !strings.Contains(got, "tool-digest 1.0 stable active verified") {
		t.Errorf("placed: %s", got)
	}
	if strings.Contains(got, "tool-extra") {
		t.Errorf("an asset with a mismatching checksum was ingested: %s", got)
	}
	cs, _ := f.st.Conflicts(context.Background(), false)
	if len(cs) != 1 || cs[0].Kind != "checksum" {
		t.Errorf("conflicts = %+v", cs)
	}
}

func TestVerifyRequiredRejectsUnverified(t *testing.T) {
	required := "required"
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool", Verify: &required})
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "x"))
	if st := f.sync(); !strings.Contains(st.LastError, "verify is required") {
		t.Errorf("LastError = %q", st.LastError)
	}
	if len(f.placed()) != 0 {
		t.Errorf("unverified asset ingested: %v", f.placed())
	}
}

func TestAssetRulesAndDuplicates(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool", Exclude: []string{"*-musl*"},
		Assets: []config.AssetRule{
			{Match: "*_ubuntu24.04.deb", Suite: "noble"},
			{Match: "*.rpm", Release: "el9"},
			{Match: "*", Suite: "stable"},
		}})
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool_1.0_amd64_ubuntu24.04.deb", debtest.Build("tool", "1.0", "amd64", "noble"))
	f.forge.add(rel, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "stable"))
	f.forge.add(rel, "tool-musl_1.0_amd64.deb", debtest.Build("tool-musl", "1.0", "amd64", "m"))
	f.forge.add(rel, "tool-dbgsym_1.0_amd64.deb", debtest.Build("tool-dbgsym", "1.0", "amd64", "d"))
	// Two files for the same package and suite: both skipped.
	f.forge.add(rel, "lib_1.0_amd64.deb", debtest.Build("lib", "1.0", "amd64", "a"))
	f.forge.add(rel, "lib_1.0_amd64.v2.deb", debtest.Build("lib", "1.0", "amd64", "b"))
	f.sync()
	got := strings.Join(f.placed(), "; ")
	for _, want := range []string{"tool 1.0 noble", "tool 1.0 stable"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, unwanted := range []string{"tool-musl", "tool-dbgsym", "lib "} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q should have been skipped: %s", unwanted, got)
		}
	}
	cs, _ := f.st.Conflicts(context.Background(), false)
	if len(cs) != 2 || cs[0].Kind != "asset-mapping" {
		t.Errorf("conflicts = %+v, want two asset-mapping conflicts", cs)
	}
}

func TestManualApprovalAndOwnership(t *testing.T) {
	manual := "manual"
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool", Approval: &manual})
	// Another owner already holds "taken".
	if err := f.st.SetOwner(context.Background(), "os", "taken", "team-x", store.OwnerFirst); err != nil {
		t.Fatal(err)
	}
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "x"))
	f.forge.add(rel, "taken_1.0_amd64.deb", debtest.Build("taken", "1.0", "amd64", "y"))
	f.sync()
	got := strings.Join(f.placed(), "; ")
	if !strings.Contains(got, "tool 1.0 stable held") || !strings.Contains(got, "taken 1.0 stable held") {
		t.Errorf("placed: %s", got)
	}
	cs, _ := f.st.Conflicts(context.Background(), false)
	if len(cs) != 1 || cs[0].Kind != "ownership" {
		t.Errorf("conflicts = %+v", cs)
	}
}

func TestOrgFilters(t *testing.T) {
	f := newFixture(t, config.Source{Name: "org", Org: "platform", Include: []string{"*"}, Exclude: []string{"legacy-*"}})
	f.forge.repos = []forge.Repo{
		{Path: "platform/app"}, {Path: "platform/legacy-x"},
		{Path: "platform/old", Archived: true}, {Path: "platform/forked", Fork: true},
		{Path: "platform/docs"},
	}
	for _, repo := range []string{"platform/app", "platform/legacy-x", "platform/old", "platform/forked"} {
		name := strings.TrimPrefix(repo, "platform/")
		rel := f.release(repo, name+"1", 1)
		f.forge.add(rel, name+"_1.0_amd64.deb", debtest.Build(name, "1.0", "amd64", name))
	}
	f.release("platform/docs", "d1", 1) // no assets
	f.sync()
	if got := strings.Join(f.placed(), "; "); got != "app 1.0 stable active unverified" {
		t.Errorf("placed %q, want only app", got)
	}
	st := f.m.Status(context.Background())
	if len(st) != 1 || strings.Join(st[0].Unmatched, ",") != "platform/docs" {
		t.Errorf("status = %+v, want platform/docs listed without assets", st)
	}
}

func TestTransientFailureRetries(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	rel := f.release("org/tool", "1", 1)
	a := f.forge.add(rel, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "x"))
	f.forge.fail[a.URL] = true
	if st := f.sync(); !strings.Contains(st.LastError, "connection reset") {
		t.Fatalf("LastError = %q", st.LastError)
	}
	delete(f.forge.fail, a.URL)
	if st := f.sync(); st.LastError != "" {
		t.Fatalf("retry failed: %q", st.LastError)
	}
	if len(f.placed()) != 1 {
		t.Errorf("release not processed on retry: %v", f.placed())
	}
}

// An ingest failure that is not the package's fault, here an unwritable
// blob store as with a full disk, leaves the release for the next poll.
func TestIngestFailureRetries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on")
	}
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "x"))
	blobTmp := filepath.Join(f.m.TmpDir, "blobs", "tmp")
	if err := os.Chmod(blobTmp, 0o555); err != nil {
		t.Fatal(err)
	}
	if st := f.sync(); !strings.Contains(st.LastError, "permission denied") {
		t.Fatalf("LastError = %q", st.LastError)
	}
	if err := os.Chmod(blobTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if st := f.sync(); st.LastError != "" {
		t.Fatalf("retry failed: %q", st.LastError)
	}
	if len(f.placed()) != 1 {
		t.Errorf("release not processed on retry: %v", f.placed())
	}
}

func TestParseSums(t *testing.T) {
	h := strings.Repeat("a", 64)
	sums := map[string]string{}
	parseSums(h+"  dist/tool.deb\n"+strings.Repeat("B", 64)+" *x.rpm\nnot a line\n", "checksums", sums)
	parseSums(h+"\n", "single.deb", sums)
	if sums["tool.deb"] != h || sums["x.rpm"] != strings.Repeat("b", 64) || sums["single.deb"] != h {
		t.Errorf("sums = %v", sums)
	}
}

func TestUnconfiguredArchIsSkippedNotFailed(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "x"))
	f.forge.add(rel, "tool_1.0_arm64.deb", debtest.Build("tool", "1.0", "arm64", "y"))
	if st := f.sync(); st.LastError != "" || st.LastSuccess == "" {
		t.Errorf("state = %+v, want success", st)
	}
	st := f.m.Status(context.Background())
	if len(st) != 1 || len(st[0].Skipped) != 1 || !strings.Contains(st[0].Skipped[0], "arm64") {
		t.Errorf("status = %+v, want the arm64 asset listed as skipped", st)
	}
}

// reload replaces the configuration, as a SIGHUP would, and applies it.
func (f *fixture) reload(edit func(c *config.Config)) {
	f.t.Helper()
	c := *f.live.Load()
	c.Sources = append([]config.Source(nil), c.Sources...)
	edit(&c)
	f.live.Store(&c)
	f.cfg = &c
	if err := f.m.Restart(); err != nil {
		f.t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReloadRestartsOnlyChangedSources(t *testing.T) {
	f := newFixture(t, config.Source{Name: "a", Repo: "org/a"})
	f.m.MaxStartDelay = time.Millisecond
	f.reload(func(c *config.Config) {
		c.Sources = append(c.Sources, config.Source{Name: "b", Forge: "gh", Repo: "org/b"})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.m.Stop()
	waitFor(t, "first polls", func() bool { return f.forge.count("org/a") == 1 && f.forge.count("org/b") == 1 })

	// Change b, add c, leave a alone.
	f.reload(func(c *config.Config) {
		c.Sources[1].Exclude = []string{"*-musl*"}
		c.Sources = append(c.Sources, config.Source{Name: "c", Forge: "gh", Repo: "org/c"})
	})
	waitFor(t, "changed and new sources to poll", func() bool { return f.forge.count("org/b") == 2 && f.forge.count("org/c") == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := f.forge.count("org/a"); n != 1 {
		t.Errorf("unchanged source polled %d times after a reload, want 1", n)
	}

	// A reload with nothing changed polls nothing.
	f.reload(func(*config.Config) {})
	time.Sleep(50 * time.Millisecond)
	if a, b, c := f.forge.count("org/a"), f.forge.count("org/b"), f.forge.count("org/c"); a != 1 || b != 2 || c != 1 {
		t.Errorf("polls after an unchanged reload: a=%d b=%d c=%d, want 1 2 1", a, b, c)
	}

	// Removing a source stops it.
	f.reload(func(c *config.Config) { c.Sources = c.Sources[:2] })
	if err := f.m.Trigger("c"); err == nil {
		t.Error("removed source can still be triggered")
	}
	if err := f.m.Trigger("a"); err != nil {
		t.Errorf("trigger a: %v", err)
	}
	waitFor(t, "triggered poll", func() bool { return f.forge.count("org/a") == 2 })
}

func TestReloadDoesNotWaitForRunningSync(t *testing.T) {
	f := newFixture(t, config.Source{Name: "a", Repo: "org/a"})
	release := make(chan struct{})
	f.forge.block = map[string]chan struct{}{"org/a": release}
	f.m.MaxStartDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.m.Stop()
	waitFor(t, "sync to start", func() bool { return f.forge.count("org/a") == 1 })

	done := make(chan struct{})
	go func() {
		f.reload(func(c *config.Config) { c.Sources[0].Exclude = []string{"x"} })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reload waited for a running sync")
	}
	close(release)
	// The replaced loop's sync was cancelled; the new loop polls again.
	waitFor(t, "new loop to poll", func() bool { return f.forge.count("org/a") >= 2 })
}

func TestSourceChecksumGlobs(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool",
		Checksums: []string{"tool_*_SHA256SUMS", "*.deb.sha256sum"}})
	a := debtest.Build("tool", "1.0", "amd64", "a")
	b := debtest.Build("tool-extra", "1.0", "amd64", "b")
	c := debtest.Build("tool-other", "1.0", "amd64", "c")
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool_1.0_amd64.deb", a)
	f.forge.add(rel, "tool-extra_1.0_amd64.deb", b)
	f.forge.add(rel, "tool-other_1.0_amd64.deb", c)
	f.forge.add(rel, "tool_1.0_SHA256SUMS", []byte(sum(a)+"  tool_1.0_amd64.deb\n"))
	f.forge.add(rel, "tool-extra_1.0_amd64.deb.sha256sum", []byte(sum(b)+"\n"))
	// A built-in name is ignored once checksums: is set.
	f.forge.add(rel, "checksums.txt", []byte(strings.Repeat("0", 64)+"  tool-other_1.0_amd64.deb\n"))
	if st := f.sync(); st.LastError != "" {
		t.Fatalf("LastError = %q", st.LastError)
	}
	got := strings.Join(f.placed(), "; ")
	for _, want := range []string{"tool 1.0 stable active verified", "tool-extra 1.0 stable active verified",
		"tool-other 1.0 stable active unverified"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

// Assets that cannot become packages are not downloaded: no enabled
// format for their extension (rpm is not registered in the fixture), or
// no asset rule matching their name. They do not fail the run either.
func TestSkipsBeforeDownload(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool", Assets: []config.AssetRule{
		{Match: "*-amd64.deb", Suite: "stable"},
		{Match: "*.rpm", Release: "stable"},
	}})
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool-1.0-amd64.deb", debtest.Build("tool", "1.0", "amd64", "a"))
	f.forge.add(rel, "tool-1.0-arm64.deb", debtest.Build("tool", "1.0", "arm64", "b"))
	f.forge.add(rel, "tool-1.0-x86_64.rpm", []byte("not read"))
	f.forge.add(rel, "tool.flatpak", []byte("not read"))
	if st := f.sync(); st.LastError != "" {
		t.Fatalf("LastError = %q", st.LastError)
	}
	if got := strings.Join(f.forge.downloads, ","); got != "tool-1.0-amd64.deb" {
		t.Errorf("downloaded %q, want only tool-1.0-amd64.deb", got)
	}
	if got := strings.Join(f.placed(), "; "); got != "tool 1.0 stable active unverified" {
		t.Errorf("placed %q", got)
	}
}

// downloadsOf counts the downloads of an asset name.
func (f *fixture) downloadsOf(name string) int {
	f.forge.mu.Lock()
	defer f.forge.mu.Unlock()
	n := 0
	for _, d := range f.forge.downloads {
		if d == name {
			n++
		}
	}
	return n
}

// conflictKinds returns "kind package" for every open conflict.
func (f *fixture) conflictKinds() []string {
	f.t.Helper()
	cs, err := f.st.Conflicts(context.Background(), false)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.Kind+" "+c.PackageRef)
	}
	return out
}

// A download that cannot succeed is skipped with a conflict instead of
// being retried on every poll, which held up every newer release.
func TestPermanentDownloadFailureIsSkipped(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture, rel *forge.Release)
		want  string // in LastError
	}{
		{"too large", func(f *fixture, rel *forge.Release) {
			big := debtest.Build("tool", "1.0", "amd64", strings.Repeat("x", 4096))
			f.cfg.Storage.MaxPackageSize = config.ByteSize(len(big) - 1)
			f.forge.add(rel, "tool_1.0_amd64.deb", big)
		}, "larger than storage.max_package_size"},
		{"gone", func(f *fixture, rel *forge.Release) {
			a := f.forge.add(rel, "tool_1.0_amd64.deb", nil)
			delete(f.forge.files, a.URL)
		}, "HTTP 404"},
		{"checksum file gone", func(f *fixture, rel *forge.Release) {
			f.forge.add(rel, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
			a := f.forge.add(rel, "SHA256SUMS", nil)
			delete(f.forge.files, a.URL)
		}, "SHA256SUMS: download"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
			r0 := f.release("org/tool", "0", 1)
			f.forge.add(r0, "tool_0.1_amd64.deb", debtest.Build("tool", "0.1", "amd64", "0"))
			f.sync()
			tt.setup(f, f.release("org/tool", "1", 2))
			r2 := f.release("org/tool", "2", 3)
			f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))

			if st := f.sync(); !strings.Contains(st.LastError, tt.want) {
				t.Errorf("LastError = %q, want it to mention %q", st.LastError, tt.want)
			}
			f.sync()
			got := strings.Join(f.placed(), "; ")
			if !strings.Contains(got, "tool 2.0") || strings.Contains(got, "tool 1.0") {
				t.Errorf("placed %q, want 2.0 and not 1.0", got)
			}
			if n := f.downloadsOf("tool_1.0_amd64.deb") + f.downloadsOf("SHA256SUMS"); n != 1 {
				t.Errorf("failing asset fetched %d times over two polls, want 1", n)
			}
			if c := f.conflictKinds(); len(c) != 1 || !strings.HasPrefix(c[0], "download tool/org/tool@v1/") {
				t.Errorf("conflicts = %v, want one download conflict for v1", c)
			}
		})
	}
}

// A release seen before its assets are attached gets them on a later poll,
// on a source's first run as well as later ones.
func TestAssetsAttachedAfterPublishing(t *testing.T) {
	for _, firstRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("first run %v", firstRun), func(t *testing.T) {
			f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
			if !firstRun {
				r0 := f.release("org/tool", "0", 1)
				f.forge.add(r0, "tool_0.1_amd64.deb", debtest.Build("tool", "0.1", "amd64", "0"))
				f.sync()
			}
			f.release("org/tool", "1", 2)
			if st := f.sync(); st.LastError != "" {
				t.Fatalf("poll without assets: %q", st.LastError)
			}
			r1 := &f.forge.releases["org/tool"][0]
			f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
			f.sync()
			if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 1.0") {
				t.Errorf("placed %q, want the late asset of release 1", got)
			}
		})
	}
}

// Only the newest release waits for assets: a newer one ends the wait, so
// a repository that never attaches assets keeps no backlog.
func TestNewerReleaseEndsWaitForAssets(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	r0 := f.release("org/tool", "0", 1)
	f.forge.add(r0, "tool_0.1_amd64.deb", debtest.Build("tool", "0.1", "amd64", "0"))
	f.sync()
	f.release("org/tool", "1", 2)
	f.sync()
	r2 := f.release("org/tool", "2", 3)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.sync()
	seen, err := f.st.SeenRelease(context.Background(), "tool", "org/tool", "1")
	if err != nil || !seen {
		t.Errorf("release 1 seen = %v, %v; want it marked once release 2 was processed", seen, err)
	}
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want release 2", got)
	}
}

// Without asset rules, assets go to the default distribution: stable if
// configured, else the first one listed.
func TestNoRulesUsesDefaultDistribution(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.cfg.Deb.Suites = []string{"noble", "jammy"}
	rel := f.release("org/tool", "1", 1)
	f.forge.add(rel, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "x"))
	if st := f.sync(); st.LastError != "" {
		t.Fatalf("LastError = %q", st.LastError)
	}
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 1.0 noble") {
		t.Errorf("placed = %s, want tool 1.0 in noble", got)
	}
}

// Releases published in the same second are all processed, also when one
// of them fails once and another is processed first.
func TestReleasesWithSameTimestamp(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	r2 := f.release("org/tool", "2", 2)
	a2 := f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	r3 := f.release("org/tool", "3", 2)
	f.forge.add(r3, "tool_3.0_amd64.deb", debtest.Build("tool", "3.0", "amd64", "3"))
	f.forge.fail[a2.URL] = true
	f.sync()
	delete(f.forge.fail, a2.URL)
	if st := f.sync(); st.LastError != "" {
		t.Fatalf("retry: %s", st.LastError)
	}
	got := strings.Join(f.placed(), "; ")
	for _, want := range []string{"tool 2.0", "tool 3.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("placed %q, want %s", got, want)
		}
	}
}

// A release whose download keeps failing does not hold up newer releases,
// and is still taken once its download works.
func TestFailingReleaseDoesNotBlockNewer(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	r2 := f.release("org/tool", "2", 2)
	a2 := f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.forge.fail[a2.URL] = true
	r3 := f.release("org/tool", "3", 3)
	f.forge.add(r3, "tool_3.0_amd64.deb", debtest.Build("tool", "3.0", "amd64", "3"))
	if st := f.sync(); !strings.Contains(st.LastError, "release v2") {
		t.Errorf("LastError = %q, want the failing release", st.LastError)
	}
	r4 := f.release("org/tool", "4", 4)
	f.forge.add(r4, "tool_4.0_amd64.deb", debtest.Build("tool", "4.0", "amd64", "4"))
	if st := f.sync(); !strings.Contains(st.LastError, "release v2") {
		t.Errorf("LastError = %q, want the failing release still reported", st.LastError)
	}
	got := strings.Join(f.placed(), "; ")
	if !strings.Contains(got, "tool 3.0") || !strings.Contains(got, "tool 4.0") || strings.Contains(got, "tool 2.0") {
		t.Fatalf("placed %q, want 3.0 and 4.0 but not 2.0", got)
	}

	delete(f.forge.fail, a2.URL)
	if st := f.sync(); st.LastError != "" {
		t.Errorf("LastError = %q after the download works", st.LastError)
	}
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want 2.0 once its download works", got)
	}
}

// A pending release is retried even when the listing has not changed
// since the poll that failed it.
func TestPendingReleaseRetriedWhenListingUnchanged(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.forge.etag = "v1"
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	r2 := f.release("org/tool", "2", 2)
	a2 := f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	r3 := f.release("org/tool", "3", 3)
	f.forge.add(r3, "tool_3.0_amd64.deb", debtest.Build("tool", "3.0", "amd64", "3"))
	f.forge.etag = "v2"
	f.forge.fail[a2.URL] = true
	f.sync()
	f.sync() // the listing is unchanged: same ETag
	delete(f.forge.fail, a2.URL)
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want 2.0 retried", got)
	}
}

// The status tells forge and registry sources apart, so the UI
// lists each under its ecosystem.
func TestStatusKind(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	cfg := *f.cfg
	cfg.Sources = append(cfg.Sources, config.Source{Name: "img", Registry: "r", Image: "a/b", Tags: []string{"*"}})
	f.live.Store(&cfg)
	kinds := map[string]string{}
	for _, s := range f.m.Status(context.Background()) {
		kinds[s.Name] = s.Kind
	}
	if kinds["tool"] != server.KindForge || kinds["img"] != server.KindRegistry {
		t.Errorf("kinds %v", kinds)
	}
}

// A forge whose client cannot be built fails only its own sources: the
// others still start, and its sources report the error until a reload
// builds the client.
func TestBrokenForgeFailsOnlyItsSources(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	cfg := *f.cfg
	cfg.Forges = append(slices.Clone(cfg.Forges), config.Forge{Name: "broken", Type: "gitlab", URL: "https://x", MaxConcurrent: 1})
	cfg.Sources = append(slices.Clone(cfg.Sources), config.Source{Name: "other", Forge: "broken", Repo: "org/other"})
	f.live.Store(&cfg)
	broken := true
	f.m.NewClient = func(fc config.Forge) (forge.Client, error) {
		if fc.Name == "broken" && broken {
			return nil, errors.New("token_file: permission denied")
		}
		return f.forge, nil
	}
	f.m.MaxStartDelay = time.Hour

	if err := f.m.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("Start: got %v, want an error naming forge broken", err)
	}
	for _, name := range []string{"tool", "other"} {
		if err := f.m.Trigger(name); err != nil {
			t.Errorf("source %s not started: %v", name, err)
		}
	}
	f.m.Stop()

	other := cfg.Sources[1]
	f.m.RunOnce(context.Background(), other)
	st, err := f.st.SourceState(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.LastError, "token_file: permission denied") {
		t.Errorf("LastError = %q, want the client error", st.LastError)
	}

	broken = false
	if err := f.m.Start(context.Background()); err != nil {
		t.Fatalf("Start after fix: %v", err)
	}
	f.m.Stop()
	f.m.RunOnce(context.Background(), other)
	if st, _ = f.st.SourceState(context.Background(), "other"); st.LastError != "" {
		t.Errorf("LastError after fix = %q, want none", st.LastError)
	}
}
