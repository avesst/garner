// Package sync polls forges for releases and ingests their package assets
// (design section 7, "Forge sync").
package sync

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	gosync "sync"
	"time"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/forge"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/id"
	"github.com/avesst/garner/internal/ingest"
	"github.com/avesst/garner/internal/metrics"
	"github.com/avesst/garner/internal/server"
	"github.com/avesst/garner/internal/store"
)

// Manager runs one poll loop per source and implements server.Syncer.
type Manager struct {
	Store    *store.Store
	Ingester *ingest.Ingester
	Formats  *format.Registry
	Config   func() *config.Config
	TmpDir   string
	Log      *slog.Logger
	// NewClient creates forge clients; tests replace it.
	NewClient func(config.Forge) (forge.Client, error)
	// MaxStartDelay bounds the random delay before a new loop's first
	// poll (default 30 s), so many sources do not poll at once.
	MaxStartDelay time.Duration
	// Now is the clock for min_release_age; tests replace it.
	Now func() time.Time

	mu       gosync.Mutex
	base     context.Context // from Start; loops derive from it
	workers  map[string]*worker
	running  map[string]bool
	repoInfo map[string][]string // source -> repos without matching assets
	skipped  map[string][]string // source -> assets skipped in the last run
	deferred map[string][]server.DeferredRelease
	// noEffect holds source/repo@tag of overrides already reported as
	// having no effect, so the warning is logged once per process.
	noEffect map[string]bool
	// notCovering maps source/repo@tag to the time of the override last
	// reported as not covering its release, so each override is reported
	// once and the map holds one entry per release.
	notCovering map[string]time.Time
	clients     map[string]forge.Client
	wg          gosync.WaitGroup

	lastSuccess *metrics.Vec
	errors      *metrics.Vec
	added       *metrics.Vec
}

// worker is one source's poll loop and the configuration it started with.
type worker struct {
	cancel context.CancelFunc
	trig   chan struct{}
	key    workerKey
}

// workerKey is everything that decides how a source is polled. A reload
// restarts a source's loop only when its key changes.
type workerKey struct {
	Source   config.Source
	Settings config.Settings
	Forge    config.Forge
}

// RegisterMetrics adds the sync metrics of section 13.
func (m *Manager) RegisterMetrics(r *metrics.Registry) {
	m.lastSuccess = r.Gauge("garner_sync_last_success_timestamp_seconds", "Unix time of the last successful sync.", "source")
	m.errors = r.Counter("garner_sync_errors_total", "Failed sync runs.", "source")
	m.added = r.Counter("garner_sync_packages_added_total", "Packages added by sync.", "source")
	r.Collect(func(emit func(string, string, map[string]string, float64)) {
		sources := m.Config().Sources
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, s := range sources {
			emit("garner_sync_releases_deferred", "Releases waiting for min_release_age.",
				map[string]string{"source": s.Name}, float64(len(m.deferred[s.Name])))
		}
		for name, c := range m.clients {
			if n := c.RateLimitRemaining(); n >= 0 {
				emit("garner_forge_ratelimit_remaining", "Remaining API quota reported by the forge.",
					map[string]string{"forge": name}, float64(n))
			}
		}
	})
}

// Start starts the poll loops for the current configuration. Loops stop
// when ctx is done.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.base = ctx
	if m.running == nil {
		m.running = map[string]bool{}
		m.repoInfo = map[string][]string{}
		m.skipped = map[string][]string{}
		m.deferred = map[string][]server.DeferredRelease{}
		m.noEffect = map[string]bool{}
		m.notCovering = map[string]time.Time{}
	}
	m.mu.Unlock()
	return m.Restart()
}

// Restart applies the current configuration after a reload. Forge
// clients are rebuilt, so rotated tokens take effect. Sources whose
// settings are unchanged keep their loop and schedule; new and changed
// sources get a new loop, removed ones are stopped. It does not wait for
// running syncs: a cancelled sync stops at its next step and its release
// stays unseen, so the next run retries it.
func (m *Manager) Restart() error {
	cfg := m.Config()
	newClient := m.NewClient
	if newClient == nil {
		newClient = forge.New
	}
	clients := map[string]forge.Client{}
	forges := map[string]config.Forge{}
	for _, f := range cfg.Forges {
		c, err := newClient(f)
		if err != nil {
			return fmt.Errorf("forge %s: %w", f.Name, err)
		}
		clients[f.Name] = c
		forges[f.Name] = f
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base == nil || m.base.Err() != nil {
		return nil
	}
	m.clients = clients
	if m.workers == nil {
		m.workers = map[string]*worker{}
	}
	want := map[string]workerKey{}
	for _, src := range cfg.Sources {
		want[src.Name] = workerKey{Source: src, Settings: cfg.Settings(src), Forge: forges[src.Forge]}
	}
	for name, w := range m.workers {
		if k, ok := want[name]; !ok || !reflect.DeepEqual(k, w.key) {
			w.cancel()
			delete(m.workers, name)
			if ok {
				m.Log.Info("source changed; restarting its poll loop", "source", name)
			} else {
				m.Log.Info("source removed; stopping its poll loop", "source", name)
			}
		}
	}
	for name, k := range want {
		if _, ok := m.workers[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(m.base)
		w := &worker{cancel: cancel, trig: make(chan struct{}, 1), key: k}
		m.workers[name] = w
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.loop(ctx, name, w.trig)
		}()
	}
	return nil
}

// Stop ends every poll loop and waits for running syncs, for shutdown.
// Restart does nothing after Stop until Start runs again.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.base = nil
	for name, w := range m.workers {
		w.cancel()
		delete(m.workers, name)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Trigger starts a sync of a source now.
func (m *Manager) Trigger(name string) error {
	m.mu.Lock()
	w, ok := m.workers[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no source named %q", name)
	}
	select {
	case w.trig <- struct{}{}:
	default: // one is already pending
	}
	return nil
}

// Status reports each configured source.
func (m *Manager) Status(ctx context.Context) []server.SourceStatus {
	var out []server.SourceStatus
	for _, s := range m.Config().Sources {
		st, err := m.Store.SourceState(ctx, s.Name)
		if err != nil {
			continue
		}
		m.mu.Lock()
		info := slices.Clone(m.repoInfo[s.Name])
		skipped := slices.Clone(m.skipped[s.Name])
		deferred := slices.Clone(m.deferred[s.Name])
		m.mu.Unlock()
		out = append(out, server.SourceStatus{Name: s.Name, LastRun: st.LastRun,
			LastSuccess: st.LastSuccess, LastError: st.LastError, Unmatched: info, Skipped: skipped,
			Deferred: deferred})
	}
	return out
}

// IgnoreAge records that a release may skip min_release_age. The override
// covers only a release published before now, so one re-created under the
// same tag later waits again.
func (m *Manager) IgnoreAge(ctx context.Context, source, repo, tag, by string) error {
	if err := m.Store.AddAgeOverride(ctx, source, repo, tag, by, m.now()); err != nil {
		return err
	}
	m.Log.Info("release may skip min_release_age", "source", source, "repo", repo, "tag", tag, "by", by)
	return nil
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) loop(ctx context.Context, name string, trig chan struct{}) {
	maxDelay := m.MaxStartDelay
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(maxDelay))))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-trig:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		cfg := m.Config()
		src, ok := findSource(cfg, name)
		if !ok {
			return
		}
		if !m.RunOnce(ctx, src) {
			// A sync from the previous loop of this source is still
			// running (a reload replaced the loop); try again soon.
			timer.Reset(maxDelay)
			continue
		}
		interval := cfg.Settings(src).PollInterval
		// ±10 % jitter (section 7).
		jitter := time.Duration((rand.Float64()*0.2 - 0.1) * float64(interval))
		timer.Reset(interval + jitter)
	}
}

func findSource(cfg *config.Config, name string) (config.Source, bool) {
	for _, s := range cfg.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return config.Source{}, false
}

// RunOnce syncs one source and records the outcome. It returns false,
// without syncing, when a sync of the source is already running.
func (m *Manager) RunOnce(ctx context.Context, src config.Source) bool {
	m.mu.Lock()
	if m.running[src.Name] {
		m.mu.Unlock()
		return false
	}
	m.running[src.Name] = true
	client := m.clients[src.Forge]
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.running, src.Name)
		m.mu.Unlock()
	}()

	log := m.Log.With("source", src.Name)
	st, err := m.Store.SourceState(ctx, src.Name)
	if err != nil {
		log.Error("read source state", "err", err)
		return true
	}
	st.LastRun = store.Now()
	run := &run{m: m, src: src, cfg: m.Config(), client: client, state: st, log: log,
		wasDeferred: map[string]bool{}, deferredRepos: map[string]bool{}}
	m.mu.Lock()
	run.prevDeferred = slices.Clone(m.deferred[src.Name])
	m.mu.Unlock()
	for _, d := range run.prevDeferred {
		run.wasDeferred[d.Repo+"@"+d.Tag] = true
	}
	err = run.sync(ctx)
	if errors.Is(err, context.Canceled) {
		log.Info("sync cancelled; unfinished releases are retried next run")
		return true
	}
	if err != nil {
		st.LastError = err.Error()
		if m.errors != nil {
			m.errors.Inc(src.Name)
		}
		log.Error("sync failed", "err", err)
	} else {
		st.LastError = ""
		st.LastSuccess = st.LastRun
		if m.lastSuccess != nil {
			m.lastSuccess.Set(float64(time.Now().Unix()), src.Name)
		}
		log.Info("sync finished", "added", run.added, "deferred", len(run.deferred))
	}
	if m.added != nil && run.added > 0 {
		m.added.Add(float64(run.added), src.Name)
	}
	if err := m.Store.SaveSourceState(context.Background(), st); err != nil {
		log.Error("save source state", "err", err)
	}
	return true
}

// run is one sync of one source.
type run struct {
	m        *Manager
	src      config.Source
	cfg      *config.Config
	client   forge.Client
	state    *store.SourceState
	log      *slog.Logger
	added    int
	skipped  []string
	deferred []server.DeferredRelease
	// prevDeferred is the previous run's list; wasDeferred holds its
	// repo@tag, so each release is logged once rather than on every poll.
	prevDeferred []server.DeferredRelease
	wasDeferred  map[string]bool
	// deferredRepos holds the repositories whose deferrals this run has
	// worked out.
	deferredRepos map[string]bool
}

func (r *run) sync(ctx context.Context) error {
	if r.client == nil {
		return fmt.Errorf("unknown forge %q", r.src.Forge)
	}
	repos, err := r.repos(ctx)
	if err != nil {
		return err
	}
	var errs []error
	var unmatched []string
	for _, repo := range repos {
		matched, err := r.syncRepo(ctx, repo)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
			if !r.deferredRepos[repo] {
				// Its releases still wait; keep showing them while the
				// forge fails.
				for _, d := range r.prevDeferred {
					if d.Repo == repo {
						r.deferred = append(r.deferred, d)
					}
				}
			}
			continue
		}
		if !matched {
			unmatched = append(unmatched, repo)
		}
	}
	r.m.mu.Lock()
	r.m.repoInfo[r.src.Name] = unmatched
	r.m.skipped[r.src.Name] = r.skipped
	r.m.deferred[r.src.Name] = r.deferred
	r.m.mu.Unlock()
	return errors.Join(errs...)
}

// repos returns the repositories to poll: the single repo, or the
// filtered listing of an organization or group.
func (r *run) repos(ctx context.Context) ([]string, error) {
	if r.src.Repo != "" {
		return []string{r.src.Repo}, nil
	}
	owner := r.src.Org
	if owner == "" {
		owner = r.src.Group
	}
	list, err := r.client.ListRepos(ctx, owner, r.cfg.Settings(r.src).Recursive)
	if err != nil {
		return nil, fmt.Errorf("list repositories of %s: %w", owner, err)
	}
	var out []string
	for _, repo := range list {
		if (repo.Archived && !r.src.Archived) || (repo.Fork && !r.src.Forks) {
			continue
		}
		base := path.Base(repo.Path)
		if len(r.src.Include) > 0 && !matchAny(r.src.Include, base) {
			continue
		}
		if matchAny(r.src.Exclude, base) {
			continue
		}
		out = append(out, repo.Path)
	}
	if err := r.m.Store.SaveRepos(ctx, r.src.Name, out); err != nil {
		return nil, err
	}
	return out, nil
}

func matchAny(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

// syncRepo processes a repository's new releases. It reports whether any
// release had candidate assets.
func (r *run) syncRepo(ctx context.Context, repo string) (bool, error) {
	q := &r.m.Store.Queries
	last, seenAny, err := q.LastProcessed(ctx, r.src.Name, repo)
	if err != nil {
		return false, err
	}
	pending, oldest, err := q.PendingReleases(ctx, r.src.Name, repo)
	if err != nil {
		return false, err
	}
	key := r.src.Forge + ":" + repo
	// Pending releases are retried, so the listing must reach them, and
	// must be fetched in full: an ETag stored before a release became
	// pending can match again once that release is pulled, and "not
	// modified" would then keep its row forever.
	since := last
	if !oldest.IsZero() && oldest.Before(since) {
		since = oldest
	}
	etagIn := r.state.ETags[key]
	if len(pending) > 0 {
		etagIn = ""
	}
	releases, etag, err := r.client.Releases(ctx, repo, since, etagIn)
	if errors.Is(err, forge.ErrNotModified) {
		return true, nil
	}
	if err != nil {
		return false, err
	}

	settings := r.cfg.Settings(r.src)
	var candidates []forge.Release
	listed := map[string]bool{}
	for _, rel := range releases {
		if rel.Draft || (rel.Prerelease && !settings.Prereleases) {
			continue
		}
		candidates = append(candidates, rel)
		listed[rel.ID] = true
	}
	// A pending release the listing does not hold is looked up on its own:
	// the listing stops after a few pages, and forges sort by creation
	// rather than publish time. One that is gone, or became a draft or
	// pre-release, is dropped, as when a compromised release is pulled
	// during min_release_age; keeping it would page back to it forever.
	var lookupErrs []error
	var stillDeferred []server.DeferredRelease
	for id := range pending {
		if listed[id] {
			continue
		}
		rel, found, err := r.client.Release(ctx, repo, id)
		if err != nil {
			// Only this release is held up: it stays pending, and the
			// rest of the repository goes ahead.
			lookupErrs = append(lookupErrs, fmt.Errorf("look up pending release %s: %w", id, err))
			// If it was waiting for its age, it still is: keep it in
			// status rather than log it as new once the forge recovers.
			for _, d := range r.prevDeferred {
				if d.Repo == repo && d.ID == id {
					stillDeferred = append(stillDeferred, d)
				}
			}
			r.log.Warn("look up pending release failed; keeping it pending", "repo", repo, "release_id", id, "err", err)
			continue
		}
		usable := found && !rel.Draft && (!rel.Prerelease || settings.Prereleases)
		if usable && rel.ID != id {
			// The forge now names it differently (GitLab IDs gained the
			// creation time): keep it pending under its current ID.
			if err := q.MarkPending(ctx, r.src.Name, repo, rel.ID, rel.PublishedAt); err != nil {
				return false, err
			}
			pending[rel.ID] = true
		}
		if usable && !listed[rel.ID] {
			candidates = append(candidates, rel)
			listed[rel.ID] = true
		}
		if usable && rel.ID == id {
			continue
		}
		if err := q.DropPending(ctx, r.src.Name, repo, id); err != nil {
			return false, err
		}
		delete(pending, id)
		switch {
		case usable:
			r.log.Info("pending release renamed by the forge", "repo", repo, "release_id", id, "now", rel.ID)
		case found:
			r.log.Warn("pending release is now a draft or pre-release; dropping it", "repo", repo, "release_id", id)
		default:
			r.log.Warn("pending release no longer published; dropping it", "repo", repo, "release_id", id)
		}
	}
	// A pending release without package assets has nothing left to wait
	// or retry for (its assets were removed). Kept pending, it would stop
	// the listing from using its ETag until a newer release.
	for _, rel := range candidates {
		if pending[rel.ID] && !r.hasCandidates(rel) {
			if err := q.DropPending(ctx, r.src.Name, repo, rel.ID); err != nil {
				return false, err
			}
			delete(pending, rel.ID)
			r.log.Warn("pending release has no package assets any more; dropping it", "repo", repo, "tag", rel.Tag)
		}
	}
	slices.SortStableFunc(candidates, func(a, b forge.Release) int { return a.PublishedAt.Compare(b.PublishedAt) })
	gated := settings.MinReleaseAge > 0
	var young []forge.Release
	overridden := map[string]bool{}
	if gated {
		candidates, young, err = r.splitByAge(ctx, repo, candidates, settings.MinReleaseAge, overridden)
		if err != nil {
			return false, err
		}
	}
	var latest string
	if len(candidates) > 0 {
		latest = candidates[len(candidates)-1].ID
	}
	waiting, err := r.deferrable(ctx, repo, young, candidates, seenAny, last, pending)
	if err != nil {
		return false, err
	}
	// A deferred release is recorded as pending, so it stays reachable when
	// a newer release, let through by an override, moves past it.
	var deferred []server.DeferredRelease
	for _, rel := range waiting {
		if err := q.MarkPending(ctx, r.src.Name, repo, rel.ID, rel.PublishedAt); err != nil {
			return false, err
		}
		d := server.DeferredRelease{ID: rel.ID, Repo: repo, Tag: rel.Tag}
		if !rel.PublishedAt.IsZero() {
			d.PublishedAt = store.FormatTime(rel.PublishedAt)
			d.EligibleAt = store.FormatTime(r.ageFrom(rel).Add(settings.MinReleaseAge))
		}
		deferred = append(deferred, d)
		if !r.wasDeferred[d.Repo+"@"+d.Tag] {
			r.log.Info("deferring release", "repo", d.Repo, "tag", d.Tag,
				"published_at", d.PublishedAt, "eligible_at", d.EligibleAt)
		}
	}
	// Status gets them now: they wait even if processing below fails.
	r.deferred = append(r.deferred, deferred...)
	r.deferred = append(r.deferred, stillDeferred...)
	r.deferredRepos[repo] = true
	candidates = slices.DeleteFunc(candidates, func(rel forge.Release) bool {
		drop := r.dropped(rel, pending, seenAny, latest, last)
		if drop && overridden[rel.ID] {
			r.overrideNoEffect(ctx, repo, rel)
		}
		return drop
	})

	matched, retry := false, false
	var errs []error
	for i, rel := range candidates {
		seen, err := r.seen(ctx, repo, rel)
		if err != nil {
			return matched, err
		}
		if seen {
			continue
		}
		if overridden[rel.ID] {
			r.log.Info("release skips min_release_age by override", "repo", repo, "tag", rel.Tag)
		}
		had, transient, err := r.syncRelease(ctx, repo, rel)
		matched = matched || had
		if err != nil {
			errs = append(errs, fmt.Errorf("release %s: %w", rel.Tag, err))
		}
		if transient {
			// Leave it unseen so the next poll retries it, without
			// holding up the newer releases (section 7).
			if err := q.MarkPending(ctx, r.src.Name, repo, rel.ID, rel.PublishedAt); err != nil {
				return matched, err
			}
			retry = true
			continue
		}
		if !had && err == nil && i == len(candidates)-1 && (!gated || rel.AssetTimes || overridden[rel.ID]) {
			// The newest release has no candidate assets yet: many CI
			// pipelines publish a release and attach assets afterwards.
			// Leave it unseen, so a later poll picks them up, until a
			// newer release replaces it. With min_release_age, assets
			// attached later wait for their own age where the forge dates
			// them. Where it does not, an old release is processed as it
			// is, so late assets cannot skip the age; one let through by
			// an override still waits, since its pipeline may be running.
			continue
		}
		if err := q.MarkSeen(ctx, r.src.Name, repo, rel.Tag, rel.ID, rel.PublishedAt); err != nil {
			return matched, err
		}
	}
	// A deferred release must be listed again, so the listing's ETag is
	// not kept: "not modified" would hide it until the next new release.
	if etag != "" && !retry && len(deferred) == 0 {
		r.state.ETags[key] = etag
	}
	return matched || len(candidates) == 0, errors.Join(append(errs, lookupErrs...)...)
}

// hasCandidates reports whether any of a release's assets could become a
// package, judged by name as before downloading.
func (r *run) hasCandidates(rel forge.Release) bool {
	return slices.ContainsFunc(rel.Assets, func(a forge.Asset) bool { return r.isCandidate(a.Name) })
}

// seen reports whether a release was processed, under its ID or the ID an
// older garner gave it.
func (r *run) seen(ctx context.Context, repo string, rel forge.Release) (bool, error) {
	q := &r.m.Store.Queries
	ok, err := q.SeenRelease(ctx, r.src.Name, repo, rel.ID)
	if err != nil || ok || rel.FormerID == "" {
		return ok, err
	}
	return q.SeenRelease(ctx, r.src.Name, repo, rel.FormerID)
}

// dropped reports whether this run leaves a listed release alone: on a first
// run every release but the latest, later only those before the last
// processed one. Pending releases are always kept.
func (r *run) dropped(rel forge.Release, pending map[string]bool, seenAny bool, latest string, last time.Time) bool {
	switch {
	case pending[rel.ID]:
		return false
	case !seenAny:
		// First run, or a newly discovered repository: only the latest.
		return rel.ID != latest
	default:
		// A release published in the same second as the last processed
		// one is kept; seen_releases skips those already processed.
		return rel.PublishedAt.Before(last)
	}
}

// overrideNoEffect warns, once per process, that an override lets a
// release through that this source never processes. A release the
// override already let through is not warned about.
func (r *run) overrideNoEffect(ctx context.Context, repo string, rel forge.Release) {
	if seen, err := r.seen(ctx, repo, rel); err != nil || seen {
		return
	}
	key := r.src.Name + "/" + repo + "@" + rel.Tag
	r.m.mu.Lock()
	logged := r.m.noEffect[key]
	r.m.noEffect[key] = true
	r.m.mu.Unlock()
	if !logged {
		r.log.Warn("override of min_release_age has no effect: the release is older than where this source starts",
			"repo", repo, "tag", rel.Tag)
	}
}

// overrideNotCovering warns, once per process and override, that an
// override exists for a release it does not cover: the release or one of
// its package assets is newer than the override, because it was re-created
// or changed since, or the forge's clock runs ahead of garner's.
func (r *run) overrideNotCovering(ctx context.Context, repo string, rel forge.Release, from, at time.Time) {
	// A processed release is never looked at again, and the server
	// refuses a new override for it.
	if seen, err := r.seen(ctx, repo, rel); err != nil || seen {
		return
	}
	key := r.src.Name + "/" + repo + "@" + rel.Tag
	r.m.mu.Lock()
	logged := r.m.notCovering[key].Equal(at)
	r.m.notCovering[key] = at
	r.m.mu.Unlock()
	if !logged {
		r.log.Warn("override of min_release_age does not cover this release: it or a package asset is newer than the override; "+
			"once its assets are attached, run garner sync --ignore-age again",
			"repo", repo, "tag", rel.Tag, "age_from", store.FormatTime(from), "override_at", store.FormatTime(at))
	}
}

// ageFrom is the time a release's age is measured from: its publish time,
// or the upload of its newest candidate asset when that is later and the
// forge dates assets. Zero means unknown.
func (r *run) ageFrom(rel forge.Release) time.Time {
	t := rel.PublishedAt
	if t.IsZero() || !rel.AssetTimes {
		return t
	}
	for _, a := range rel.Assets {
		if r.isCandidate(a.Name) && a.CreatedAt.After(t) {
			t = a.CreatedAt
		}
	}
	return t
}

// splitByAge separates the releases old enough for min_release_age, or let
// through by an override, from the younger ones. A release without a
// publish time is never old enough. An override covers a release only if
// it and its candidate assets predate the override, so a release re-created
// under the same tag, or an asset attached later, waits again. Overridden
// releases are added to overridden.
func (r *run) splitByAge(ctx context.Context, repo string, rels []forge.Release, minAge time.Duration,
	overridden map[string]bool) (ready, young []forge.Release, err error) {
	now := r.m.now()
	for _, rel := range rels {
		from := r.ageFrom(rel)
		if !from.IsZero() && now.Sub(from) >= minAge {
			ready = append(ready, rel)
			continue
		}
		at, ok, err := r.m.Store.AgeOverride(ctx, r.src.Name, repo, rel.Tag)
		if err != nil {
			return nil, nil, err
		}
		if ok && from.Before(at) {
			overridden[rel.ID] = true
			ready = append(ready, rel)
			continue
		}
		if ok {
			r.overrideNotCovering(ctx, repo, rel, from, at)
		}
		young = append(young, rel)
	}
	return ready, young, nil
}

// deferrable returns the young releases a later run will process once
// they are old enough: not processed yet, and not older than what this
// run starts from (the last processed release, or on a first run the
// latest ready one). Pending releases and those without a publish time
// are kept regardless of that point.
func (r *run) deferrable(ctx context.Context, repo string, young, ready []forge.Release, seenAny bool,
	last time.Time, pending map[string]bool) ([]forge.Release, error) {
	from, hasFrom := last, seenAny
	if !seenAny && len(ready) > 0 {
		from, hasFrom = ready[len(ready)-1].PublishedAt, true
	}
	var out []forge.Release
	for _, rel := range young {
		if hasFrom && !pending[rel.ID] && !rel.PublishedAt.IsZero() && rel.PublishedAt.Before(from) {
			continue
		}
		// Nothing in it can be published, so there is nothing to wait
		// for; assets attached later change the listing and wait then.
		if !r.hasCandidates(rel) {
			continue
		}
		seen, err := r.seen(ctx, repo, rel)
		if err != nil {
			return nil, err
		}
		if !seen {
			out = append(out, rel)
		}
	}
	return out, nil
}

// asset is a downloaded candidate asset.
type asset struct {
	forge.Asset
	path   string
	format format.Format
	parsed *format.Parsed
	target string // distribution
	verify string
}

// syncRelease downloads, verifies and ingests one release's assets.
// transient reports a failure worth retrying (network, forge errors).
func (r *run) syncRelease(ctx context.Context, repo string, rel forge.Release) (matched, transient bool, err error) {
	var cands []forge.Asset
	for _, a := range rel.Assets {
		if r.isCandidate(a.Name) {
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		return false, false, nil
	}
	tmp, err := os.MkdirTemp(r.m.TmpDir, "sync-")
	if err != nil {
		return true, true, err
	}
	defer os.RemoveAll(tmp)

	sums, err := r.checksums(ctx, rel, tmp)
	if err != nil {
		var ce *checksumError
		if errors.As(err, &ce) && permanent(err) {
			// The assets cannot be verified: skip the release rather
			// than retry it on every poll or take them unverified.
			r.conflict("download", repo, rel, ce.name, err.Error())
			return true, false, err
		}
		return true, true, err
	}
	settings := r.cfg.Settings(r.src)
	var errs []error
	var ready []*asset
	for _, a := range cands {
		da, err := r.download(ctx, a, tmp)
		if err != nil {
			err = fmt.Errorf("%s: %w", a.Name, err)
			if !permanent(err) {
				return true, true, err
			}
			// Retrying cannot help, and would hold up every newer
			// release of the repository.
			errs = append(errs, err)
			r.conflict("download", repo, rel, a.Name, err.Error())
			continue
		}
		if want, ok := expected(a, sums); ok {
			if want != da.sha256 {
				errs = append(errs, fmt.Errorf("%s: checksum mismatch: got %s, want %s", a.Name, da.sha256, want))
				r.conflict("checksum", repo, rel, a.Name, "checksum mismatch")
				continue
			}
			da.verify = "verified"
		} else if settings.Verify == "required" {
			errs = append(errs, fmt.Errorf("%s: no checksum available and verify is required", a.Name))
			continue
		} else {
			da.verify = "unverified"
		}
		if err := r.identify(da.asset); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name, err))
			continue
		}
		if da.target == "" {
			continue // no asset rule targets this file
		}
		ready = append(ready, da.asset)
	}

	// Two assets mapping to the same package and distribution are both
	// skipped; garner never guesses (section 7).
	type key struct{ format, name, version, arch, dist string }
	groups := map[key][]*asset{}
	for _, a := range ready {
		v := fmt.Sprintf("%d:%s-%s", a.parsed.Epoch, a.parsed.Version, a.parsed.Release)
		k := key{a.format.Name(), a.parsed.Name, v, a.parsed.Arch, a.target}
		groups[k] = append(groups[k], a)
	}
	for _, a := range ready {
		v := fmt.Sprintf("%d:%s-%s", a.parsed.Epoch, a.parsed.Version, a.parsed.Release)
		g := groups[key{a.format.Name(), a.parsed.Name, v, a.parsed.Arch, a.target}]
		if len(g) > 1 {
			var names []string
			for _, x := range g {
				names = append(names, x.Name)
			}
			r.conflict("asset-mapping", repo, rel, a.Name, "assets map to the same package and distribution: "+strings.Join(names, ", "))
			continue
		}
		if err := r.ingest(ctx, repo, rel, a, settings); err != nil {
			err = fmt.Errorf("%s: %w", a.Name, err)
			var ie *ingest.Error
			if !errors.As(err, &ie) {
				// Not the package's fault (disk, database, signing):
				// leave the release unseen so the next poll retries it.
				return true, true, errors.Join(append(errs, err)...)
			}
			errs = append(errs, err)
		}
	}
	return true, false, errors.Join(errs...)
}

// isCandidate decides from the name alone, before downloading, whether an
// asset can become a package: an enabled format uses its extension, the
// fixed filters of section 7 and the source's exclude list do not drop
// it, and, when the source has asset rules, at least one rule matches the
// name (an asset no rule matches is skipped anyway).
func (r *run) isCandidate(name string) bool {
	if !r.m.Formats.HandlesName(name) || strings.HasSuffix(name, ".src.rpm") ||
		strings.Contains(name, "-dbgsym") || strings.Contains(name, "-debuginfo") ||
		matchAny(r.src.Exclude, name) {
		return false
	}
	if len(r.src.Assets) == 0 {
		return true
	}
	for _, rule := range r.src.Assets {
		if ok, _ := path.Match(rule.Match, name); ok {
			return true
		}
	}
	return false
}

// errTooLarge is an asset over storage.max_package_size.
var errTooLarge = errors.New("larger than storage.max_package_size")

// permanent reports whether a download failure recurs on every retry: the
// asset is too large, or the server says it is gone.
func permanent(err error) bool {
	var se *forge.StatusError
	return errors.Is(err, errTooLarge) || errors.As(err, &se) && se.Permanent()
}

type downloaded struct {
	*asset
	sha256 string
}

func (r *run) download(ctx context.Context, a forge.Asset, dir string) (*downloaded, error) {
	body, err := r.client.Download(ctx, a)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	p := filepath.Join(dir, "asset-"+id.New())
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limit := int64(r.cfg.Storage.MaxPackageSize)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("%w (%d bytes)", errTooLarge, limit)
	}
	return &downloaded{asset: &asset{Asset: a, path: p}, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

// identify parses the file and picks its distribution from the asset
// rules: the first rule matching the name that sets a target for the
// file's format. With no rules, the target is the format's default
// distribution.
func (r *run) identify(a *asset) error {
	f, err := os.Open(a.path)
	if err != nil {
		return err
	}
	defer f.Close()
	fm, err := r.m.Formats.Detect(f)
	if err != nil {
		return err
	}
	parsed, err := fm.Parse(f)
	if err != nil {
		return err
	}
	a.format, a.parsed = fm, parsed
	if len(r.src.Assets) == 0 {
		a.target = format.DefaultDistribution(fm.Distributions(r.cfg))
		return nil
	}
	for _, rule := range r.src.Assets {
		ok, _ := path.Match(rule.Match, a.Name)
		if !ok {
			continue
		}
		t := map[string]string{"suite": rule.Suite, "release": rule.Release}[fm.DistributionKey()]
		if t != "" {
			a.target = t
			return nil
		}
	}
	return nil
}

func (r *run) ingest(ctx context.Context, repo string, rel forge.Release, a *asset, settings config.Settings) error {
	f, err := os.Open(a.path)
	if err != nil {
		return err
	}
	defer f.Close()
	res, err := r.m.Ingester.Ingest(ctx, f, ingest.Request{
		Targets:             map[string]string{a.format.DistributionKey(): a.target},
		Owner:               ingest.SourceOwner(r.src.Name),
		Origin:              "source",
		OriginRef:           fmt.Sprintf("%s/%s@%s/%s", r.src.Name, repo, rel.Tag, a.Name),
		VerifyStatus:        a.verify,
		Held:                settings.Approval == "manual",
		HoldOnOwnerConflict: true,
	})
	if err != nil {
		var ie *ingest.Error
		if errors.As(err, &ie) && ie.Kind == ingest.NotConfigured {
			// Expected for multi-architecture releases: shown in status,
			// not a failure (section 7).
			r.skipped = append(r.skipped, fmt.Sprintf("%s@%s/%s: %s", repo, rel.Tag, a.Name, ie.Msg))
			r.log.Info("skipped asset", "asset", a.Name, "repo", repo, "reason", ie.Msg)
			return nil
		}
		if errors.As(err, &ie) && ie.Kind == ingest.Conflict {
			r.conflict("content", repo, rel, a.Name, ie.Msg)
		}
		return err
	}
	if !res.Existing {
		r.added++
		r.log.Info("added package", "asset", a.Name, "repo", repo, "tag", rel.Tag, "state", res.State)
	}
	return nil
}

func (r *run) conflict(kind, repo string, rel forge.Release, assetName, detail string) {
	ref := fmt.Sprintf("%s/%s@%s/%s", r.src.Name, repo, rel.Tag, assetName)
	if err := r.m.Store.AddConflict(context.Background(), id.New(), kind, ref, detail); err != nil {
		r.log.Error("record conflict", "err", err)
	}
}

// checksums reads the release's checksum files into a map from file name
// to sha256.
func (r *run) checksums(ctx context.Context, rel forge.Release, dir string) (map[string]string, error) {
	sums := map[string]string{}
	for _, a := range rel.Assets {
		if !r.isChecksumFile(a.Name) {
			continue
		}
		body, err := r.client.Download(ctx, a)
		if err != nil {
			return nil, &checksumError{name: a.Name, err: err}
		}
		data, err := io.ReadAll(io.LimitReader(body, 4<<20))
		body.Close()
		if err != nil {
			return nil, err
		}
		parseSums(string(data), strings.TrimSuffix(a.Name, path.Ext(a.Name)), sums)
	}
	return sums, nil
}

// checksumError is a checksum file that could not be downloaded.
type checksumError struct {
	name string
	err  error
}

func (e *checksumError) Error() string { return e.name + ": " + e.err.Error() }
func (e *checksumError) Unwrap() error { return e.err }

// isChecksumFile matches the source's checksums: globs, or without them
// the common names: checksums.txt, *_checksums.txt (GoReleaser),
// SHA256SUMS and *.sha256, ignoring case.
func (r *run) isChecksumFile(name string) bool {
	if len(r.src.Checksums) > 0 {
		return matchAny(r.src.Checksums, name)
	}
	lower := strings.ToLower(name)
	return lower == "checksums.txt" || lower == "sha256sums" ||
		strings.HasSuffix(lower, "_checksums.txt") || strings.HasSuffix(lower, ".sha256")
}

// parseSums reads "HEX  NAME" or "HEX *NAME" lines. A line holding only a
// hash, as in a per-file tool.deb.sha256, applies to defaultName: the
// checksum file's name without its last extension.
func parseSums(data, defaultName string, sums map[string]string) {
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || len(f[0]) != 64 {
			continue
		}
		h := strings.ToLower(f[0])
		if _, err := hex.DecodeString(h); err != nil {
			continue
		}
		if len(f) >= 2 {
			sums[path.Base(strings.TrimPrefix(f[1], "*"))] = h
		} else {
			sums[defaultName] = h
		}
	}
}

// expected returns the checksum an asset must match: the forge's digest,
// or one from a checksum file.
func expected(a forge.Asset, sums map[string]string) (string, bool) {
	if a.SHA256 != "" {
		return strings.ToLower(a.SHA256), true
	}
	h, ok := sums[a.Name]
	return h, ok
}
