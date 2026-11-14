package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/avesst/garner/internal/app"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/rpm"
	"github.com/avesst/garner/internal/signing"
	gsync "github.com/avesst/garner/internal/sync"
)

// extraFormats returns the formats enabled besides deb. rpm needs
// createrepo_c, rpmsign and gpg; without them it is disabled with a
// warning. signer is resolved after the app is opened.
func extraFormats(signer func() signing.Signer, stderr io.Writer) []format.Format {
	f, err := rpm.NewFormat(signer)
	if err != nil {
		fmt.Fprintf(stderr, "garner: rpm support disabled: %v\n", err)
		return nil
	}
	return []format.Format{f}
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
