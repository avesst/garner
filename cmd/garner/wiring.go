package main

import (
	"context"
	"io"

	"github.com/avesst/garner/internal/app"
	"github.com/avesst/garner/internal/format"
)

// extraFormats returns the formats enabled besides deb.
func extraFormats(stderr io.Writer) []format.Format { return nil }

// startSync attaches forge sync to the app.
func startSync(a *app.App) error { return nil }

// backgroundLoops are run alongside the server.
func backgroundLoops(a *app.App) []func(context.Context) { return nil }
