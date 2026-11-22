package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/avesst/garner/internal/publish"
)

// verifyRetry is how often garner verify --wait asks again.
var verifyRetry = 2 * time.Second

// defaultVerifyWait is how long --config-hash waits without --wait.
const defaultVerifyWait = 5 * time.Minute

// runVerify asks the server to check the published tree. With --wait or
// --config-hash it retries while the server answers 409: a publish is
// pending, or another configuration is loaded. It exits 1 on errors, and
// with --strict on warnings too.
func runVerify(c *client, wait time.Duration, configHash string, strict bool,
	print func(any, func()), stdout io.Writer, fail func(error) int) int {
	q := url.Values{}
	if configHash != "" {
		q.Set("config_hash", configHash)
		if wait == 0 {
			wait = defaultVerifyWait
		}
	}
	if wait > 0 {
		q.Set("ready", "1")
	}
	deadline := time.Now().Add(wait)
	var rep publish.VerifyReport
	for {
		err := c.do(http.MethodPost, "/api/v1/verify", q, nil, &rep)
		var ae *apiError
		if wait > 0 && errors.As(err, &ae) && ae.status == http.StatusConflict {
			if time.Now().Add(verifyRetry).Before(deadline) {
				time.Sleep(verifyRetry)
				continue
			}
			return fail(fmt.Errorf("gave up after %s: %w", wait, err))
		}
		if err != nil {
			return fail(err)
		}
		break
	}
	print(rep, func() { printVerify(stdout, &rep) })
	if rep.Errors > 0 || strict && rep.Warnings > 0 {
		return 1
	}
	return 0
}

func printVerify(w io.Writer, rep *publish.VerifyReport) {
	if rep.Snapshot != "" {
		fmt.Fprintf(w, "snapshot %s (configuration %.12s), %d packages\n", rep.Snapshot, rep.SnapshotConfigHash, rep.Packages)
	}
	if len(rep.Problems) == 0 {
		fmt.Fprintln(w, "no problems found")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, p := range rep.Problems {
		where := ""
		if p.Path != "" {
			where = p.Path + ": "
		}
		fmt.Fprintf(tw, "%s\t%s\t%s%s\n", p.Severity, p.Check, where, p.Message)
	}
	tw.Flush()
	fmt.Fprintf(w, "%s, %s\n", plural(rep.Errors, "error"), plural(rep.Warnings, "warning"))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
