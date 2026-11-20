package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/avesst/garner/internal/app"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/deb"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/rpm"
	"github.com/avesst/garner/internal/signing"
	gsync "github.com/avesst/garner/internal/sync"
)

// formats builds a format for each configured section. A configured rpm
// needs createrepo_c, rpmsign, gpg and gpgconf; without them garner does
// not start. signer is resolved after the app is opened.
func formats(signer func() signing.Signer) app.FormatsFunc {
	return func(cfg *config.Config) ([]format.Format, error) {
		var out []format.Format
		if cfg.Deb != nil {
			out = append(out, deb.Format{})
		}
		if cfg.RPM != nil {
			f, err := rpm.NewFormat(signer)
			if err != nil {
				return nil, fmt.Errorf("rpm is configured: %w (install them, or remove the rpm section)", err)
			}
			out = append(out, f)
		}
		return out, nil
	}
}

var syncManager *gsync.Manager

// startSync attaches forge sync to the app. The poll loops start with the
// server and restart after every successful reload.
func startSync(a *app.App) error {
	cfg := a.Config()
	syncManager = &gsync.Manager{
		Store: a.Store, Ingester: a.Ingester, Formats: a.Formats, Config: a.Config,
		TmpDir: filepath.Join(cfg.Storage.Path, "tmp"), Log: a.Log,
	}
	syncManager.RegisterMetrics(a.Metrics)
	a.Syncer = syncManager
	a.OnReload = append(a.OnReload, func(*config.Config) {
		if err := syncManager.Restart(); err != nil {
			a.Log.Error("sync not restarted after reload", "err", err)
		}
	})
	return nil
}

// backgroundLoops are run alongside the server.
func backgroundLoops(a *app.App) []func(context.Context) {
	return []func(context.Context){func(ctx context.Context) {
		if err := syncManager.Start(ctx); err != nil {
			a.Log.Error("sync not started", "err", err)
		}
		<-ctx.Done()
		syncManager.Stop()
	}}
}
