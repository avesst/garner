package main

import (
	"context"
	"fmt"
	"io"

	"github.com/avesst/garner/internal/app"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/rpm"
	"github.com/avesst/garner/internal/signing"
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

// startSync attaches forge sync to the app.
func startSync(a *app.App) error { return nil }

// backgroundLoops are run alongside the server.
func backgroundLoops(a *app.App) []func(context.Context) { return nil }
