package app

import (
	"context"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/avesst/garner/internal/store"
)

func (a *App) registerMetrics() {
	m := a.Metrics
	a.publishDuration = m.Gauge("garner_publish_duration_seconds", "Duration of the last successful publish.")
	a.publishLast = m.Gauge("garner_publish_last_success_timestamp_seconds", "Unix time of the last successful publish.")
	a.publishErrors = m.Counter("garner_publish_errors_total", "Failed publishes.")
	m.Collect(func(emit func(string, string, map[string]string, float64)) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pkgs, err := a.Store.Placed(ctx, store.PackageFilter{States: []string{store.Active}})
		if err == nil {
			type key struct{ format, dist, arch string }
			counts := map[key]int{}
			for _, p := range pkgs {
				counts[key{p.Format, p.Distribution, p.Arch}]++
			}
			for k, n := range counts {
				emit("garner_packages", "Active packages by format, distribution and architecture.",
					map[string]string{"format": k.format, "distribution": k.dist, "arch": k.arch}, float64(n))
			}
		}
		if cs, err := a.Store.Conflicts(ctx, false); err == nil {
			emit("garner_conflicts_open", "Open conflicts: deliveries held for a decision.", nil, float64(len(cs)))
		}
		if counts, err := a.Store.ConflictCounts(ctx); err == nil {
			for _, kind := range slices.Sorted(maps.Keys(counts)) {
				emit("garner_conflicts_total", "Conflicts recorded, by kind.", map[string]string{"kind": kind}, float64(counts[kind]))
			}
		}
		if snaps, err := a.Store.Snapshots(ctx); err == nil {
			emit("garner_snapshots", "Snapshots on disk.", nil, float64(len(snaps)))
		}
		var size int64
		filepath.WalkDir(filepath.Join(a.Config().Storage.Path, "blobs", "sha256"), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					size += info.Size()
				}
			}
			return nil
		})
		emit("garner_storage_bytes", "Bytes in the blob store.", nil, float64(size))
		if days, ok := a.Keyring().ExpiryDays(); ok {
			emit("garner_signing_key_expiry_days", "Days until the signing key expires.", nil, days)
		}
	})
}
