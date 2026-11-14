// Package app wires garner's parts together for the serve and init
// commands.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/avesst/garner/internal/blob"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/metrics"
	"github.com/avesst/garner/internal/publish"
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
	Log       *slog.Logger
	Version   string

	// Syncer is set by the sync package when sources are configured.
	Syncer server.Syncer

	keys     atomic.Pointer[signing.Keyring]
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

// NewLogger builds the logger from the log section.
func NewLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	level.UnmarshalText([]byte(cfg.Log.Level))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Log.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// Open loads the configuration, takes the database lock and builds every
// part. Formats are the enabled formats; deb is always enabled.
func Open(ctx context.Context, configPath, version string, extra ...format.Format) (*App, error) {
	h, err := config.NewHolder(configPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	cfg := h.Current()
	log := NewLogger(cfg)
	a := &App{Holder: h, Log: log, Version: version, Metrics: metrics.NewRegistry()}

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
	if a.Formats, err = format.NewRegistry(append([]format.Format{deb.Format{}}, extra...)...); err != nil {
		return nil, err
	}
	a.refreshCommit()

	a.Scheduler = &publish.Scheduler{Delay: PublishDelay}
	a.Ingester = &ingest.Ingester{
		Store: a.Store, Blobs: a.Blobs, Formats: a.Formats, Config: a.Config,
		TmpDir: filepath.Join(cfg.Storage.Path, "tmp"), Publish: a.Scheduler.Request, Log: log,
	}
	a.Publisher = &publish.Publisher{
		Store: a.Store, Blobs: a.Blobs, Formats: a.Formats, Config: a.Config, Signer: a.Signer,
		ConfigCommit: a.ConfigCommit, Root: cfg.Storage.Path, Log: log,
	}
	a.Scheduler.Publish = a.publishAndCollect
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
		if len(restart) > 0 {
			return fmt.Errorf("changing %s needs a restart", strings.Join(restart, ", "))
		}
		keys, err := signing.Load(KeySources(new))
		if err != nil {
			return fmt.Errorf("signing key: %w", err)
		}
		if old := a.keys.Swap(keys); old.Fingerprint() != keys.Fingerprint() {
			go a.resign()
		}
		return nil
	})
	if err != nil {
		a.Log.Error("reload rejected; keeping the previous configuration", "err", err)
		return err
	}
	a.refreshCommit()
	a.Log.Info("configuration reloaded", "hash", cfg.Hash)
	for _, fn := range a.OnReload {
		fn(cfg)
	}
	// Suites, architectures, keys and the landing page may have changed.
	a.Scheduler.Request()
	return nil
}

// resign re-signs packages after the signing key changed, then publishes.
func (a *App) resign() {
	a.Log.Info("signing key changed; re-signing packages", "fingerprint", a.Signer().Fingerprint())
	n, err := a.Ingester.Resign(context.Background())
	if err != nil {
		a.Log.Error("re-signing failed", "err", err, "done", n)
	} else {
		a.Log.Info("re-signed packages", "count", n)
	}
	a.Scheduler.Request()
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

	// The first start publishes, so the repository and its key exist.
	if _, err := a.Store.CurrentSnapshot(ctx); errors.Is(err, store.ErrNotFound) {
		if err := a.publishAndCollect(ctx); err != nil {
			return err
		}
	}

	srv := &http.Server{Addr: cfg.Server.Listen, Handler: a.Server().Handler(), ReadHeaderTimeout: 30 * time.Second}
	servers := []*http.Server{srv}
	if cfg.Metrics.Listen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", a.Metrics.Handler())
		servers = append(servers, &http.Server{Addr: cfg.Metrics.Listen, Handler: mux, ReadHeaderTimeout: 30 * time.Second})
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			a.Log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	var wg sync.WaitGroup
	run := func(fn func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); fn(ctx) }()
	}
	run(a.Scheduler.Run)
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
