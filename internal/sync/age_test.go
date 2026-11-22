package sync

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/forge"
	"github.com/avesst/garner/internal/metrics"
	"github.com/avesst/garner/internal/server"
)

// gate turns on min_release_age and sets the clock to midnight of day.
func (f *fixture) gate(age time.Duration, day int) *time.Time {
	f.cfg.Defaults.MinReleaseAge = age
	now := time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC)
	f.m.Now = func() time.Time { return now }
	return &now
}

func (f *fixture) deferred() []server.DeferredRelease {
	f.t.Helper()
	for _, s := range f.m.Status(context.Background()) {
		if s.Name == f.cfg.Sources[0].Name {
			return s.Deferred
		}
	}
	f.t.Fatal("source missing from status")
	return nil
}

func TestMinReleaseAgeDefersYoungRelease(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 5)
	f.forge.etag = "v1"
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 4)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))

	reg := metrics.NewRegistry()
	f.m.RegisterMetrics(reg)
	st := f.sync()
	var buf strings.Builder
	reg.Write(&buf)
	if !strings.Contains(buf.String(), `garner_sync_releases_deferred{source="tool"} 1`) {
		t.Errorf("metrics lack one deferred release:\n%s", buf.String())
	}
	// The first run takes the latest release that is old enough.
	if got := strings.Join(f.placed(), "; "); got != "tool 1.0 stable active unverified" {
		t.Fatalf("placed %q, want only 1.0", got)
	}
	d := f.deferred()
	if len(d) != 1 || d[0].Tag != "v2" || d[0].EligibleAt != "2026-11-07T00:00:00.000000Z" {
		t.Fatalf("deferred = %+v, want v2 eligible on Nov 7", d)
	}
	if _, ok := st.ETags["gh:org/tool"]; ok {
		t.Error("ETag stored while a release is deferred; the next poll would not see it")
	}

	f.sync() // still too young
	if got := strings.Join(f.placed(), "; "); strings.Contains(got, "tool 2.0") {
		t.Fatalf("placed %q before release 2 is old enough", got)
	}
	*now = now.Add(48 * time.Hour)
	st = f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want 2.0 once old enough", got)
	}
	if d := f.deferred(); len(d) != 0 {
		t.Errorf("deferred = %+v, want none", d)
	}
	if st.ETags["gh:org/tool"] != "v1" {
		t.Errorf("ETag = %q, want it stored once nothing is deferred", st.ETags["gh:org/tool"])
	}
}

func TestMinReleaseAgeFirstRunWithNothingOldEnough(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 3)
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 2)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))

	f.sync()
	if got := f.placed(); len(got) != 0 {
		t.Fatalf("placed %q, want nothing", got)
	}
	if d := f.deferred(); len(d) != 2 {
		t.Fatalf("deferred = %+v, want both releases", d)
	}
	*now = now.Add(24 * time.Hour) // release 1 is old enough, 2 is not
	f.sync()
	if got := strings.Join(f.placed(), "; "); got != "tool 1.0 stable active unverified" {
		t.Fatalf("placed %q, want 1.0", got)
	}
	*now = now.Add(24 * time.Hour)
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want 2.0", got)
	}
}

func TestIgnoreAge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 4)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	if got := f.placed(); len(got) != 0 {
		t.Fatalf("placed %q, want nothing yet", got)
	}

	// An override for v2 before v2 exists must not cover it later, as when
	// upstream deletes a release and re-creates it under the same tag.
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v1", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v2", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if got := strings.Join(f.placed(), "; "); got != "tool 1.0 stable active unverified" {
		t.Fatalf("placed %q, want 1.0 through the override", got)
	}

	*now = now.Add(12 * time.Hour)
	r2 := f.release("org/tool", "2", 5)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.sync()
	if got := strings.Join(f.placed(), "; "); strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q: a release published after its override skipped the wait", got)
	}
	if d := f.deferred(); len(d) != 1 || d[0].Tag != "v2" {
		t.Errorf("deferred = %+v, want v2", d)
	}
}

func TestMinReleaseAgeWithoutPublishTime(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.gate(time.Hour, 5)
	r1 := f.release("org/tool", "1", 1)
	r1.PublishedAt = time.Time{}
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	d := f.deferred()
	if len(d) != 1 || d[0].EligibleAt != "" || d[0].PublishedAt != "" {
		t.Fatalf("deferred = %+v, want v1 without times", d)
	}
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v1", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if got := strings.Join(f.placed(), "; "); got != "tool 1.0 stable active unverified" {
		t.Errorf("placed %q, want 1.0 through the override", got)
	}
}

// On a forge that does not date assets (GitLab links), a release old
// enough is processed as it is: assets attached later would skip the age,
// so it is not left open.
func TestMinReleaseAgeEndsWaitForUndatedAssets(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.gate(72*time.Hour, 6)
	r0 := f.release("org/tool", "0", 1)
	f.forge.add(r0, "tool_0.1_amd64.deb", debtest.Build("tool", "0.1", "amd64", "0"))
	f.sync()
	f.release("org/tool", "1", 2)
	f.sync()
	seen, err := f.st.SeenRelease(context.Background(), "tool", "org/tool", "1")
	if err != nil || !seen {
		t.Fatalf("release 1 seen = %v, %v; want it marked without assets", seen, err)
	}
	r1 := &f.forge.releases["org/tool"][0]
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	if got := strings.Join(f.placed(), "; "); strings.Contains(got, "tool 1.0") {
		t.Errorf("placed %q: an asset attached to an old release skipped the age", got)
	}
}

func TestMinReleaseAgePerSource(t *testing.T) {
	off := time.Duration(0)
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool", MinReleaseAge: &off})
	f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 5)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	if got := strings.Join(f.placed(), "; "); got != "tool 1.0 stable active unverified" {
		t.Errorf("placed %q; the source's 0s should turn the default off", got)
	}
}

// Each deferred release is logged once, when first deferred, and the run's
// summary counts them.
func TestDeferredReleaseLoggedOnce(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	var buf bytes.Buffer
	f.m.Log = slog.New(slog.NewTextHandler(&buf, nil))
	f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 4)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	f.sync()
	logs := buf.String()
	if n := strings.Count(logs, `msg="deferring release"`); n != 1 {
		t.Errorf("deferring release logged %d times, want once:\n%s", n, logs)
	}
	if !strings.Contains(logs, "tag=v1") || !strings.Contains(logs, "eligible_at=2026-11-07T00:00:00.000000Z") {
		t.Errorf("deferring release line lacks tag or eligible_at:\n%s", logs)
	}
	if n := strings.Count(logs, "deferred=1"); n != 2 {
		t.Errorf("sync finished with deferred=1 logged %d times, want 2:\n%s", n, logs)
	}
}

// addAt attaches an asset uploaded at midnight of day, on a forge that
// dates assets.
func (f *fixture) addAt(rel *forge.Release, name string, body []byte, day float64) {
	f.forge.add(rel, name, body)
	rel.AssetTimes = true
	rel.Assets[len(rel.Assets)-1].CreatedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration((day - 1) * 24 * float64(time.Hour)))
}

// On a forge that dates assets, an asset attached to an old release waits
// for its own age, and the release stays open for it meanwhile.
func TestMinReleaseAgeGatesLateAsset(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 6)
	r0 := f.release("org/tool", "0", 1)
	f.addAt(r0, "tool_0.1_amd64.deb", debtest.Build("tool", "0.1", "amd64", "0"), 1)
	f.sync()
	r1 := f.release("org/tool", "1", 2)
	r1.AssetTimes = true
	f.sync() // old enough, no assets yet: left open
	if seen, _ := f.st.SeenRelease(context.Background(), "tool", "org/tool", "1"); seen {
		t.Fatal("release 1 marked seen without assets on a forge that dates them")
	}
	r1 = &f.forge.releases["org/tool"][0]
	f.addAt(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"), 6)
	f.sync()
	if got := strings.Join(f.placed(), "; "); strings.Contains(got, "tool 1.0") {
		t.Fatalf("placed %q: an asset attached today skipped the age", got)
	}
	if d := f.deferred(); len(d) != 1 || d[0].EligibleAt != "2026-11-09T00:00:00.000000Z" {
		t.Fatalf("deferred = %+v, want v1 eligible three days after its asset", d)
	}
	*now = now.Add(72 * time.Hour)
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 1.0") {
		t.Errorf("placed %q, want 1.0 once its asset is old enough", got)
	}
}

// An override taken while CI is still uploading must not lose the release:
// it stays open, and assets uploaded after the override wait for their age
// or a new override.
func TestIgnoreAgeBeforeAssetsAttached(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 5)
	r0 := f.release("org/tool", "0", 1)
	f.addAt(r0, "tool_0.1_amd64.deb", debtest.Build("tool", "0.1", "amd64", "0"), 1)
	f.sync()
	r1 := f.release("org/tool", "1", 5)
	r1.AssetTimes = true
	*now = now.Add(time.Hour)
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v1", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if seen, _ := f.st.SeenRelease(ctx, "tool", "org/tool", "1"); seen {
		t.Fatal("overridden release marked seen before its assets arrived")
	}
	*now = now.Add(time.Hour)
	r1 = &f.forge.releases["org/tool"][0]
	f.addAt(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"), 5+2.0/24)
	f.sync()
	if got := strings.Join(f.placed(), "; "); strings.Contains(got, "tool 1.0") {
		t.Fatalf("placed %q: an asset uploaded after the override skipped the age", got)
	}
	*now = now.Add(time.Hour)
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v1", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 1.0") {
		t.Errorf("placed %q, want 1.0 after the second override", got)
	}
}

// Where assets carry no time, an override taken before they exist keeps
// the release open, and covers the assets once they arrive.
func TestIgnoreAgeBeforeUndatedAssets(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.gate(72*time.Hour, 5)
	r0 := f.release("org/tool", "0", 1)
	f.forge.add(r0, "tool_0.1_amd64.deb", debtest.Build("tool", "0.1", "amd64", "0"))
	f.sync()
	f.release("org/tool", "1", 4)
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v1", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	r1 := &f.forge.releases["org/tool"][0]
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 1.0") {
		t.Errorf("placed %q, want 1.0 through the override", got)
	}
}

// Overriding a newer release must not drop an older one still waiting.
func TestIgnoreAgeKeepsOlderDeferred(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 5)
	f.forge.etag = "v1"
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 4)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	r3 := f.release("org/tool", "3", 5)
	f.forge.add(r3, "tool_3.0_amd64.deb", debtest.Build("tool", "3.0", "amd64", "3"))
	*now = now.Add(time.Hour)
	f.sync()
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v3", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 3.0") || strings.Contains(got, "tool 2.0") {
		t.Fatalf("placed %q, want 3.0 through the override and 2.0 still waiting", got)
	}
	if d := f.deferred(); len(d) != 1 || d[0].Tag != "v2" {
		t.Fatalf("deferred = %+v, want v2 still listed", d)
	}
	*now = now.Add(72 * time.Hour)
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want 2.0 once old enough", got)
	}
}

// A release without a publish time after processed ones stays listed as
// deferred and can be let through by an override.
func TestNoPublishTimeAfterProcessed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	r2 := f.release("org/tool", "2", 1)
	r2.PublishedAt = time.Time{}
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.sync()
	if d := f.deferred(); len(d) != 1 || d[0].Tag != "v2" {
		t.Fatalf("deferred = %+v, want v2", d)
	}
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v2", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want 2.0 through the override", got)
	}
}

// A release pulled upstream while it waits leaves no pending row behind,
// whether it is deleted or turned into a draft.
func TestPulledDeferredReleaseIsDropped(t *testing.T) {
	for how, msg := range map[string]string{"deleted": "no longer published", "drafted": "now a draft or pre-release"} {
		t.Run(how, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
			var buf bytes.Buffer
			f.m.Log = slog.New(slog.NewTextHandler(&buf, nil))
			f.gate(72*time.Hour, 5)
			r1 := f.release("org/tool", "1", 1)
			f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
			r2 := f.release("org/tool", "2", 4)
			f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
			f.sync()
			if ids, _, _ := f.st.PendingReleases(ctx, "tool", "org/tool"); !ids["2"] {
				t.Fatalf("pending = %v, want release 2", ids)
			}
			if how == "deleted" {
				f.forge.releases["org/tool"] = f.forge.releases["org/tool"][1:]
			} else {
				f.forge.releases["org/tool"][0].Draft = true
			}
			f.sync()
			if ids, oldest, _ := f.st.PendingReleases(ctx, "tool", "org/tool"); len(ids) != 0 || !oldest.IsZero() {
				t.Errorf("pending = %v (oldest %v), want none", ids, oldest)
			}
			if d := f.deferred(); len(d) != 0 {
				t.Errorf("deferred = %+v, want none", d)
			}
			if !strings.Contains(buf.String(), msg) {
				t.Errorf("log lacks %q:\n%s", msg, buf.String())
			}
		})
	}
}

// An override that lets through a release this source never processes is
// reported once; one that let its release through is not reported.
func TestOverrideWithoutEffectWarnedOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	var buf bytes.Buffer
	f.m.Log = slog.New(slog.NewTextHandler(&buf, nil))
	f.forge.wholePage = true
	now := f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 3)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 4)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	for _, tag := range []string{"v1", "v2"} {
		if err := f.m.IgnoreAge(ctx, "tool", "org/tool", tag, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	f.sync() // first run: only the latest, v2; v1 is never processed
	f.sync()
	if got := strings.Join(f.placed(), "; "); got != "tool 2.0 stable active unverified" {
		t.Fatalf("placed %q, want only 2.0", got)
	}
	if n := strings.Count(buf.String(), "has no effect"); n != 1 {
		t.Fatalf("no-effect warning logged %d times, want once:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "tag=v1") {
		t.Errorf("warning does not name v1:\n%s", buf.String())
	}

	// v3 passes v2 by override while v2 is still young: v2 was let
	// through, so its override is not reported.
	buf.Reset()
	*now = now.Add(time.Hour)
	r3 := f.release("org/tool", "3", 5)
	f.forge.add(r3, "tool_3.0_amd64.deb", debtest.Build("tool", "3.0", "amd64", "3"))
	*now = now.Add(time.Hour)
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v3", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	f.sync()
	if strings.Contains(buf.String(), "has no effect") {
		t.Errorf("warned about an override that was used:\n%s", buf.String())
	}
}

// A pulled release must be dropped even when the listing goes back to
// content whose ETag garner stored before the release appeared.
func TestPulledReleaseWithStaleETag(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.gate(72*time.Hour, 5)
	f.forge.etag = "E0"
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	if st := f.sync(); st.ETags["gh:org/tool"] != "E0" {
		t.Fatalf("ETag = %q, want E0 stored", st.ETags["gh:org/tool"])
	}
	f.forge.etag = "E1"
	r2 := f.release("org/tool", "2", 4)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.sync()
	f.forge.releases["org/tool"] = f.forge.releases["org/tool"][1:]
	f.forge.etag = "E0" // the listing is what it was before v2
	f.sync()
	if ids, _, _ := f.st.PendingReleases(ctx, "tool", "org/tool"); len(ids) != 0 {
		t.Errorf("pending = %v, want the pulled release dropped", ids)
	}
}

// While a repository's listing fails, its deferred releases stay in status
// and are not logged as newly deferred once the forge recovers.
func TestDeferredKeptWhileForgeFails(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	var buf bytes.Buffer
	f.m.Log = slog.New(slog.NewTextHandler(&buf, nil))
	f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 4)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	f.forge.listFail = map[string]bool{"org/tool": true}
	if st := f.sync(); st.LastError == "" {
		t.Fatal("sync did not fail")
	}
	if d := f.deferred(); len(d) != 1 || d[0].Tag != "v1" {
		t.Errorf("deferred during the outage = %+v, want v1", d)
	}
	f.forge.listFail = nil
	f.sync()
	if n := strings.Count(buf.String(), `msg="deferring release"`); n != 1 {
		t.Errorf("deferring release logged %d times, want once", n)
	}
}

// An override that no longer covers its release, here because a package
// asset was uploaded after it, is reported once.
func TestOverrideNotCoveringWarnedOnce(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	var buf bytes.Buffer
	f.m.Log = slog.New(slog.NewTextHandler(&buf, nil))
	f.gate(72*time.Hour, 5)
	r0 := f.release("org/tool", "0", 1)
	f.addAt(r0, "tool_0.1_amd64.deb", debtest.Build("tool", "0.1", "amd64", "0"), 1)
	f.sync()
	r1 := f.release("org/tool", "1", 4)
	f.addAt(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"), 5.5)
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v1", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	f.sync()
	if got := strings.Join(f.placed(), "; "); strings.Contains(got, "tool 1.0") {
		t.Fatalf("placed %q: the override covered an asset uploaded after it", got)
	}
	if n := strings.Count(buf.String(), "does not cover this release"); n != 1 {
		t.Errorf("not-covering warning logged %d times, want once:\n%s", n, buf.String())
	}
}

// A pending release the listing does not reach is looked up by ID and
// processed, not dropped as if pulled.
func TestPendingReleaseOutsideListing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 4)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.sync()
	f.forge.unlisted = map[string]bool{"2": true}
	f.sync() // still young: stays pending
	if ids, _, _ := f.st.PendingReleases(ctx, "tool", "org/tool"); !ids["2"] {
		t.Fatalf("pending = %v, want release 2 kept", ids)
	}
	*now = now.Add(72 * time.Hour)
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want 2.0 found by ID", got)
	}
}

// A pending release the forge now names differently (GitLab IDs gained
// the creation time) stays pending under its new ID and is processed,
// even once it is older than the last processed release.
func TestPendingReleaseRenamedByForge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 3)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	r3 := f.release("org/tool", "3", 4)
	f.forge.add(r3, "tool_3.0_amd64.deb", debtest.Build("tool", "3.0", "amd64", "3"))
	f.sync()
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v3", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync() // v3 through the override; v2 pending, now before the last processed
	for i := range f.forge.releases["org/tool"] {
		if f.forge.releases["org/tool"][i].ID == "2" {
			f.forge.releases["org/tool"][i].ID = "2b"
		}
	}
	f.forge.renamed = map[string]string{"2": "2b"}
	f.forge.unlisted = map[string]bool{"2b": true}
	*now = now.Add(72 * time.Hour)
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 2.0") {
		t.Errorf("placed %q, want 2.0 under its new ID", got)
	}
}

// A failing lookup holds up only its own release.
func TestPendingLookupFailureKeepsRepoGoing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	now := f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 4)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.sync()
	f.forge.unlisted = map[string]bool{"2": true}
	f.forge.lookFail = map[string]bool{"2": true}
	*now = now.Add(72 * time.Hour)
	r3 := f.release("org/tool", "3", 5)
	f.forge.add(r3, "tool_3.0_amd64.deb", debtest.Build("tool", "3.0", "amd64", "3"))
	st := f.sync()
	if !strings.Contains(st.LastError, "look up pending release 2") {
		t.Errorf("last error = %q, want the failed lookup", st.LastError)
	}
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 3.0") {
		t.Errorf("placed %q, want 3.0 despite the failed lookup", got)
	}
	if ids, _, _ := f.st.PendingReleases(ctx, "tool", "org/tool"); !ids["2"] {
		t.Errorf("pending = %v, want release 2 kept", ids)
	}
}

// No "run --ignore-age again" advice for a release already processed: the
// server refuses that.
func TestNoCoverWarningForProcessedRelease(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	var buf bytes.Buffer
	f.m.Log = slog.New(slog.NewTextHandler(&buf, nil))
	f.forge.wholePage = true
	f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 4)
	f.addAt(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"), 4)
	if err := f.m.IgnoreAge(ctx, "tool", "org/tool", "v1", "admin"); err != nil {
		t.Fatal(err)
	}
	f.sync()
	if got := strings.Join(f.placed(), "; "); !strings.Contains(got, "tool 1.0") {
		t.Fatalf("placed %q, want 1.0 through the override", got)
	}
	// A rename on GitHub moves the asset's time past the override.
	f.forge.releases["org/tool"][0].Assets[0].CreatedAt = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f.sync()
	if strings.Contains(buf.String(), "does not cover") {
		t.Errorf("warned about a processed release:\n%s", buf.String())
	}
}

// A release without package assets is not deferred: nothing in it can be
// published, and keeping it pending would cost the listing its ETag.
func TestAssetlessReleaseNotDeferred(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.gate(72*time.Hour, 5)
	f.forge.etag = "E1"
	r1 := f.release("org/tool", "1", 1)
	f.addAt(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"), 1)
	r2 := f.release("org/tool", "2", 4)
	f.addAt(r2, "README.md", []byte("docs"), 4)
	st := f.sync()
	if d := f.deferred(); len(d) != 0 {
		t.Errorf("deferred = %+v, want none", d)
	}
	if ids, _, _ := f.st.PendingReleases(ctx, "tool", "org/tool"); len(ids) != 0 {
		t.Errorf("pending = %v, want none", ids)
	}
	if st.ETags["gh:org/tool"] != "E1" {
		t.Errorf("ETag = %q, want it stored", st.ETags["gh:org/tool"])
	}
}

// A deferred release whose package asset is removed upstream is no longer
// pending.
func TestPendingReleaseLosingAssetsIsDropped(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 4)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.sync()
	if ids, _, _ := f.st.PendingReleases(ctx, "tool", "org/tool"); !ids["2"] {
		t.Fatalf("pending = %v, want release 2", ids)
	}
	f.forge.releases["org/tool"][0].Assets = nil
	f.sync()
	if ids, _, _ := f.st.PendingReleases(ctx, "tool", "org/tool"); len(ids) != 0 {
		t.Errorf("pending = %v, want none", ids)
	}
}

// A release processed under the ID an older garner gave it is not
// processed again under its new ID.
func TestFormerIDCountsAsProcessed(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	f.sync()
	before := len(f.forge.downloads)
	f.forge.releases["org/tool"][0].ID = "1:2026-11-01T00:00:00Z"
	f.forge.releases["org/tool"][0].FormerID = "1"
	f.sync()
	if got := len(f.forge.downloads); got != before {
		t.Errorf("downloads = %d, want %d: the release was processed again under its new ID", got, before)
	}
}

// A deferred release whose lookup fails stays in status, and is not logged
// as newly deferred once the forge answers again.
func TestDeferredKeptWhileLookupFails(t *testing.T) {
	f := newFixture(t, config.Source{Name: "tool", Repo: "org/tool"})
	var buf bytes.Buffer
	f.m.Log = slog.New(slog.NewTextHandler(&buf, nil))
	f.gate(72*time.Hour, 5)
	r1 := f.release("org/tool", "1", 1)
	f.forge.add(r1, "tool_1.0_amd64.deb", debtest.Build("tool", "1.0", "amd64", "1"))
	r2 := f.release("org/tool", "2", 4)
	f.forge.add(r2, "tool_2.0_amd64.deb", debtest.Build("tool", "2.0", "amd64", "2"))
	f.sync()
	f.forge.unlisted = map[string]bool{"2": true}
	f.forge.lookFail = map[string]bool{"2": true}
	f.sync()
	if d := f.deferred(); len(d) != 1 || d[0].Tag != "v2" {
		t.Errorf("deferred while the lookup fails = %+v, want v2", d)
	}
	f.forge.unlisted, f.forge.lookFail = nil, nil
	f.sync()
	if n := strings.Count(buf.String(), `msg="deferring release"`); n != 1 {
		t.Errorf("deferring release logged %d times, want once", n)
	}
}
