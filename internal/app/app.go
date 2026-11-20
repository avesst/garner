// Package app wires garner's parts together for the serve and init
// commands.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/metrics"
	"github.com/avesst/garner/internal/publish"
	"github.com/avesst/garner/internal/sdnotify"
	"github.com/avesst/garner/internal/server"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

// PublishDelay debounces publish requests (section 3).
const PublishDelay = 10 * time.Second

// gcInterval is how often GC runs besides after publishes.
const gcInterval = time.Hour

// App is a running garner instance.
type App struct {
	Holder    *config.Holder
	DB        *db.DB
	Store     *store.Store
	Blobs     *blob.Store
	Formats   *format.Registry
	Ingester  *ingest.Ingester
	Publisher *publish.Publisher
	Scheduler *publish.Scheduler
	Metrics   *metrics.Registry
	// Resigner runs resignIfNeeded, one run at a time, after the start
	// and after reloads.
	Resigner *publish.Scheduler
	Log      *slog.Logger
	Version  string

	// Syncer is set by the sync package when sources are configured.
	Syncer server.Syncer

	keys     atomic.Pointer[signing.Keyring]
	logBase  atomic.Pointer[slog.Handler]
	reloadMu sync.Mutex
	commit   atomic.Pointer[string]
	// OnReload is called after a successful reload.
	OnReload []func(*config.Config)

	publishDuration *metrics.Vec
	publishLast     *metrics.Vec
	publishErrors   *metrics.Vec
}

// Config returns the active configuration.
func (a *App) Config() *config.Config { return a.Holder.Current() }

// Signer returns the current keyring.
func (a *App) Signer() signing.Signer { return a.keys.Load() }

// Keyring returns the current keyring with its private-key export.
func (a *App) Keyring() *signing.Keyring { return a.keys.Load() }

// DefaultKeyPath is where init writes a generated key when signing.keys is
// empty.
func DefaultKeyPath(cfg *config.Config) string {
	return filepath.Join(cfg.Storage.Path, "signing", "key.asc")
}

// KeySources returns the configured keys, or the generated default key.
func KeySources(cfg *config.Config) []signing.KeySource {
	if len(cfg.Signing.Keys) == 0 {
		return []signing.KeySource{{KeyFile: DefaultKeyPath(cfg)}}
	}
	var out []signing.KeySource
	for _, k := range cfg.Signing.Keys {
		out = append(out, signing.KeySource{KeyFile: k.KeyFile, PassphraseFile: k.PassphraseFile})
	}
	return out
}

// newLogHandler builds the handler for the log section.
func newLogHandler(cfg *config.Config) slog.Handler {
	var level slog.Level
	level.UnmarshalText([]byte(cfg.Log.Level))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Log.Format == "text" {
		return slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.NewJSONHandler(os.Stdout, opts)
}

// logHandler passes records to the handler of the current log section, so
// a reload also reaches loggers derived before it.
type logHandler struct {
	cur *atomic.Pointer[slog.Handler]
	// with re-applies the attributes and groups of derived loggers.
	with func(slog.Handler) slog.Handler
}

func (h logHandler) current() slog.Handler {
	base := *h.cur.Load()
	if h.with != nil {
		return h.with(base)
	}
	return base
}

func (h logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return (*h.cur.Load()).Enabled(ctx, l)
}

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.current().Handle(ctx, r)
}

func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.derive(func(b slog.Handler) slog.Handler { return b.WithAttrs(attrs) })
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return h.derive(func(b slog.Handler) slog.Handler { return b.WithGroup(name) })
}

func (h logHandler) derive(f func(slog.Handler) slog.Handler) logHandler {
	prev := h.with
	h.with = func(b slog.Handler) slog.Handler {
		if prev != nil {
			b = prev(b)
		}
		return f(b)
	}
	return h
}

// FormatsFunc returns the formats a configuration enables, one per
// configured format section.
type FormatsFunc func(*config.Config) ([]format.Format, error)

// Open loads the configuration, takes the database lock and builds every
// part. formats builds the enabled formats from the configuration.
func Open(ctx context.Context, configPath, version string, formats FormatsFunc) (*App, error) {
	h, err := config.NewHolder(configPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	cfg := h.Current()
	a := &App{Holder: h, Version: version, Metrics: metrics.NewRegistry()}
	base := newLogHandler(cfg)
	a.logBase.Store(&base)
	log := slog.New(logHandler{cur: &a.logBase})
	a.Log = log

	for _, d := range []string{"blobs", "pool", "snapshots", "public", "tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.Storage.Path, d), 0o755); err != nil {
			return nil, err
		}
	}
	dbPath, err := cfg.SQLitePath()
	if err != nil {
		return nil, err
	}
	if a.DB, err = db.Open(ctx, dbPath); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			a.DB.Close()
		}
	}()
	a.Store = store.New(a.DB.DB)
	if a.Blobs, err = blob.Open(filepath.Join(cfg.Storage.Path, "blobs")); err != nil {
		return nil, err
	}
	keys, err := signing.Load(KeySources(cfg))
	if err != nil {
		return nil, fmt.Errorf("signing key: %w (run garner init to generate one)", err)
	}
	a.keys.Store(keys)
	fms, err := formats(cfg)
	if err != nil {
		return nil, err
	}
	if a.Formats, err = format.NewRegistry(fms...); err != nil {
		return nil, err
	}
	if got := a.Formats.Names(); !slices.Equal(got, cfg.Formats()) {
		return nil, fmt.Errorf("formats %v do not match the configured sections %v", got, cfg.Formats())
	}
	a.refreshCommit()

	a.Scheduler = &publish.Scheduler{Delay: PublishDelay}
	a.Ingester = &ingest.Ingester{
		Store: a.Store, Blobs: a.Blobs, Formats: a.Formats, Config: a.Config,
		TmpDir: filepath.Join(cfg.Storage.Path, "tmp"), Publish: a.Scheduler.Request, Log: log,
	}
	build, err := publish.BuildID()
	if err != nil {
		log.Warn("cannot identify the garner build; a rebuild with the same version will not republish on start", "err", err)
	}
	a.Publisher = &publish.Publisher{
		Store: a.Store, Blobs: a.Blobs, Formats: a.Formats, Config: a.Config, Signer: a.Signer,
		ConfigCommit: a.ConfigCommit, Version: version, Build: build, Root: cfg.Storage.Path, Log: log,
	}
	a.Scheduler.Publish = a.publishAndCollect
	a.Resigner = &publish.Scheduler{Publish: a.resignIfNeeded}
	a.registerMetrics()
	if err := a.Publisher.Reconcile(ctx); err != nil {
		return nil, err
	}
	ok = true
	return a, nil
}

// Close releases the database and its lock.
func (a *App) Close() error { return a.DB.Close() }

// ConfigCommit returns the git commit of the configuration file, or "".
func (a *App) ConfigCommit() string {
	if c := a.commit.Load(); c != nil {
		return *c
	}
	return ""
}

// refreshCommit records the commit of the configuration file if it is in a
// clean git working tree (section 5).
func (a *App) refreshCommit() {
	commit := ""
	dir := filepath.Dir(a.Holder.Path())
	if git, err := exec.LookPath("git"); err == nil {
		out, err := exec.Command(git, "-C", dir, "log", "-1", "--format=%H", "--", filepath.Base(a.Holder.Path())).Output()
		if err == nil {
			commit = strings.TrimSpace(string(out))
			if st, err := exec.Command(git, "-C", dir, "status", "--porcelain", "--", filepath.Base(a.Holder.Path())).Output(); err == nil && len(st) > 0 {
				commit += " (modified)"
			}
		}
	}
	a.commit.Store(&commit)
}

// Reload re-reads the configuration and keys. Changes that need a restart
// are rejected and the old configuration stays active.
func (a *App) Reload() error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	cfg, err := a.Holder.ReloadWith(func(old, new *config.Config) error {
		var restart []string
		if old.Storage.Path != new.Storage.Path {
			restart = append(restart, "storage.path")
		}
		if old.Database.URL != new.Database.URL {
			restart = append(restart, "database.url")
		}
		if old.Server.Listen != new.Server.Listen {
			restart = append(restart, "server.listen")
		}
		if old.Metrics.Listen != new.Metrics.Listen {
			restart = append(restart, "metrics.listen")
		}
		// Formats are built once at start, so adding or removing a
		// format section takes a restart.
		if !slices.Equal(old.Formats(), new.Formats()) {
			restart = append(restart, "the format sections")
		}
		if len(restart) > 0 {
			return fmt.Errorf("changing %s needs a restart", strings.Join(restart, ", "))
		}
		keys, err := signing.Load(KeySources(new))
		if err != nil {
			return fmt.Errorf("signing key: %w", err)
		}
		a.keys.Store(keys)
		return nil
	})
	if err != nil {
		a.Log.Error("reload rejected; keeping the previous configuration", "err", err)
		return err
	}
	base := newLogHandler(cfg)
	a.logBase.Store(&base)
	a.refreshCommit()
	a.Log.Info("configuration reloaded", "hash", cfg.Hash)
	for _, fn := range a.OnReload {
		fn(cfg)
	}
	// Suites, architectures, keys and the landing page may have changed.
	a.Resigner.Request()
	a.Scheduler.Request()
	return nil
}

// unconfiguredFormats returns the stored formats that have no format
// registered, with their package counts.
func (a *App) unconfiguredFormats(ctx context.Context) (map[string]store.FormatCount, error) {
	counts, err := a.Store.FormatCounts(ctx)
	if err != nil {
		return nil, err
	}
	for name := range counts {
		if _, ok := a.Formats.Get(name); ok {
			delete(counts, name)
		}
	}
	return counts, nil
}

// warnUnconfiguredFormats logs stored packages that are kept but not
// served, because their format's section is missing.
func (a *App) warnUnconfiguredFormats(ctx context.Context) {
	missing, err := a.unconfiguredFormats(ctx)
	if err != nil {
		a.Log.Error("cannot list stored formats", "err", err)
		return
	}
	for _, name := range slices.Sorted(maps.Keys(missing)) {
		a.Log.Warn("packages of a format that is not configured are kept but not served; add its section to serve them",
			"format", name, "packages", missing[name].Total)
	}
}

// resignIfNeeded re-signs packages when the signing key differs from
// the one they were last signed with: after a reload or a restart with a
// new key, or after a re-sign that did not finish. Then it publishes.
func (a *App) resignIfNeeded(ctx context.Context) error {
	fp := a.Signer().Fingerprint()
	prev, err := a.Store.SignedWith(ctx)
	if err != nil {
		a.Log.Error("cannot read which key packages are signed with", "err", err)
		return err
	}
	if prev == fp {
		return nil
	}
	a.Log.Info("signing key changed; re-signing packages", "fingerprint", fp, "previous", prev)
	n, err := a.Ingester.Resign(ctx)
	if err != nil {
		a.Log.Error("re-signing failed", "err", err, "done", n)
		return err
	}
	a.Log.Info("re-signed packages", "count", n)
	// Re-signed packages of a format that is not configured were skipped.
	// Recording the key would mark them done, so they are re-signed once
	// their section is back. Formats whose packages are not re-signed do
	// not hold it back.
	missing, err := a.unconfiguredFormats(ctx)
	if err != nil {
		a.Log.Error("cannot list stored formats", "err", err)
		return err
	}
	var pending []string
	for _, name := range slices.Sorted(maps.Keys(missing)) {
		if missing[name].Signed > 0 {
			pending = append(pending, name)
		}
	}
	if len(pending) > 0 {
		a.Log.Warn("signing key not recorded: packages of formats that are not configured still need re-signing",
			"formats", pending)
	} else if err := a.Store.SetSignedWith(ctx, fp); err != nil {
		a.Log.Error("cannot record the signing key", "err", err)
		return err
	}
	if n > 0 {
		a.Scheduler.Request()
	}
	return nil
}

func (a *App) publishAndCollect(ctx context.Context) error {
	start := time.Now()
	_, err := a.Publisher.Publish(ctx)
	if err != nil {
		a.publishErrors.Inc()
		a.Log.Error("publish failed", "err", err)
		return err
	}
	a.publishDuration.Set(time.Since(start).Seconds())
	a.publishLast.Set(float64(time.Now().Unix()))
	if _, err := a.Publisher.GC(ctx); err != nil {
		a.Log.Error("gc failed", "err", err)
	}
	return nil
}

// Server returns the HTTP server for this app.
func (a *App) Server() *server.Server {
	return &server.Server{
		Store: a.Store, Formats: a.Formats, Ingester: a.Ingester, Publisher: a.Publisher,
		Scheduler: a.Scheduler, Syncer: a.Syncer, Config: a.Config, ConfigCommit: a.ConfigCommit,
		Reload: a.Reload, Metrics: a.Metrics, Root: a.Config().Storage.Path, Version: a.Version,
		Log: a.Log, MetricsSeparate: a.Config().Metrics.Listen != "",
	}
}

// Run serves until SIGINT or SIGTERM; SIGHUP reloads. Background loops
// (publish scheduler, GC, extra) stop with it.
func (a *App) Run(ctx context.Context, background ...func(context.Context)) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := a.Config()

	// The first start publishes before serving, so the repository and its
	// key exist. Later starts publish soon after only if something the
	// current snapshot was rendered from changed while garner was stopped:
	// enabled formats, keys and the binary are not in the configuration
	// hash.
	if _, err := a.Store.CurrentSnapshot(ctx); errors.Is(err, store.ErrNotFound) {
		if err := a.publishAndCollect(ctx); err != nil {
			return err
		}
	} else if stale, err := a.Publisher.Stale(ctx); err != nil {
		a.Log.Warn("could not compare with the current snapshot; publishing", "err", err)
		a.Scheduler.Request()
	} else if stale {
		a.Log.Info("inputs changed since the current snapshot; publishing")
		a.Scheduler.Request()
	} else {
		a.Log.Info("current snapshot is up to date")
	}

	srv := server.NewHTTPServer(cfg.Server.Listen, a.Server().Handler())
	servers := []*http.Server{srv}
	if cfg.Metrics.Listen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", a.Metrics.Handler())
		servers = append(servers, server.NewHTTPServer(cfg.Metrics.Listen, mux))
	}
	// Under systemd, listeners stored by the previous run of the service
	// are reused, so connections queue during a restart (sdnotify.Listen).
	files := sdnotify.Files()
	names := []string{"http", "metrics"}
	listeners := make([]net.Listener, len(servers))
	for i, s := range servers {
		l, err := sdnotify.Listen(names[i], s.Addr, files, a.Log)
		if err != nil {
			for _, l := range listeners[:i] {
				l.Close()
			}
			return fmt.Errorf("listen on %s: %w", s.Addr, err)
		}
		listeners[i] = l
	}
	// Anything left was stored for a listener no longer configured.
	for name, fs := range files {
		for _, f := range fs {
			f.Close()
		}
		sdnotify.Remove(name)
	}
	errc := make(chan error, len(servers))
	for i, s := range servers {
		go func() {
			a.Log.Info("listening", "addr", listeners[i].Addr())
			if err := s.Serve(listeners[i]); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	sdnotify.Notify("READY=1")

	var wg sync.WaitGroup
	run := func(fn func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); fn(ctx) }()
	}
	a.warnUnconfiguredFormats(ctx)
	run(a.Scheduler.Run)
	run(a.Resigner.Run)
	a.Resigner.Request() // the key may have changed while garner was stopped
	run(a.gcLoop)
	for _, fn := range background {
		run(fn)
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	var err error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-hup:
			a.Reload()
		case err = <-errc:
			break loop
		}
	}
	a.Log.Info("shutting down")
	sdnotify.Notify("STOPPING=1")
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(shutdown)
	}
	stop()
	wg.Wait()
	return err
}

func (a *App) gcLoop(ctx context.Context) {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := a.Publisher.GC(ctx); err != nil {
				a.Log.Error("gc failed", "err", err)
			}
		}
	}
}
