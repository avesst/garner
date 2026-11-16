package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/avesst/garner/internal/server"
	"github.com/avesst/garner/internal/store"
)

// client calls garner's API.
type client struct {
	base   string
	name   string
	secret string
	http   *http.Client
}

func newClient(base string) (*client, error) {
	if base == "" {
		base = "http://localhost:8080"
	}
	c := &client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 0}}
	if tok := os.Getenv("GARNER_TOKEN"); tok != "" {
		name, secret, ok := strings.Cut(tok, ":")
		if !ok {
			return nil, fmt.Errorf("GARNER_TOKEN must be NAME:SECRET")
		}
		c.name, c.secret = name, secret
	}
	return c, nil
}

// secureTransport reports whether credentials sent to u stay off the
// network in clear: https, or plain http to loopback (design section 11).
func secureTransport(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// do sends a request and decodes a JSON response into out. Errors carry
// the server's message.
func (c *client) do(method, path string, query url.Values, body io.Reader, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return err
	}
	if c.secret != "" {
		if !secureTransport(req.URL) && os.Getenv("GARNER_INSECURE_AUTH") != "1" {
			return fmt.Errorf("refusing to send the token in clear text to %s; use https, "+
				"or set GARNER_INSECURE_AUTH=1 on an isolated network", req.URL.Host)
		}
		req.SetBasicAuth(c.name, c.secret)
	}
	if body != nil && method != http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func runAPI(cmd string, args []string, stdout, stderr io.Writer) int {
	return runAPIWith(os.Getenv("GARNER_URL"), cmd, args, stdout, stderr)
}

func runAPIWith(base, cmd string, args []string, stdout, stderr io.Writer) int {
	fs := newFlags(cmd, stderr)
	urlFlag := fs.String("url", base, "garner URL (default $GARNER_URL or http://localhost:8080)")
	suite := fs.String("suite", "", "deb suite (upload)")
	release := fs.String("release", "", "rpm release (upload)")
	version := fs.String("version", "", "version (remove)")
	arch := fs.String("arch", "", "architecture (remove)")
	dist := fs.String("distribution", "", "distribution (remove, packages list)")
	name := fs.String("name", "", "name (packages list, token create)")
	owner := fs.String("owner", "", "owner (token create)")
	scope := fs.String("scope", "", "scopes (token create)")
	all := fs.Bool("all", false, "include resolved or removed entries")
	asJSON := fs.Bool("json", false, "print the raw JSON response")
	pos, err := parse(fs, args)
	if err != nil {
		return 2
	}
	c, err := newClient(*urlFlag)
	if err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 2
	}
	sub := ""
	if len(pos) > 0 {
		sub = pos[0]
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "garner %s: %v\n", cmd, err)
		return 1
	}
	usageErr := func(u string) int {
		fmt.Fprintf(stderr, "Usage: garner %s\n", u)
		return 2
	}
	print := func(v any, table func()) {
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			enc.Encode(v)
			return
		}
		table()
	}

	switch cmd {
	case "upload":
		if len(pos) == 0 {
			return usageErr("upload [--suite S] [--release R] FILES...")
		}
		q := url.Values{}
		if *suite != "" {
			q.Set("suite", *suite)
		}
		if *release != "" {
			q.Set("release", *release)
		}
		failed := 0
		for _, path := range pos {
			f, err := os.Open(path)
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", path, err)
				failed++
				continue
			}
			var res server.UploadResponse
			err = c.do(http.MethodPut, "/api/v1/upload", q, f, &res)
			f.Close()
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", filepath.Base(path), err)
				failed++
				continue
			}
			what := "uploaded"
			if res.Existing {
				what = "already present"
			}
			fmt.Fprintf(stdout, "%s: %s as %s %s %s in %s (%s)\n", filepath.Base(path), what,
				res.Package.Name, res.Package.Version, res.Package.Arch, res.Package.Distribution, res.Package.State)
		}
		if failed > 0 {
			return 1
		}
	case "remove":
		if len(pos) != 1 {
			return usageErr("remove NAME [--version V] [--arch A] [--distribution D]")
		}
		q := url.Values{}
		for k, v := range map[string]string{"version": *version, "arch": *arch, "distribution": *dist} {
			if v != "" {
				q.Set(k, v)
			}
		}
		var out []server.PackageJSON
		if err := c.do(http.MethodDelete, "/api/v1/packages/"+url.PathEscape(pos[0]), q, nil, &out); err != nil {
			return fail(err)
		}
		print(out, func() { printPackages(stdout, out) })
	case "owner":
		switch {
		case sub == "list":
			var out []store.NameOwner
			if err := c.do(http.MethodGet, "/api/v1/owners", nil, nil, &out); err != nil {
				return fail(err)
			}
			print(out, func() {
				tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tOWNER\tSOURCE\tASSIGNED")
				for _, o := range out {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", o.Name, o.Owner, o.Source, o.AssignedAt)
				}
				tw.Flush()
			})
		case sub == "set" && len(pos) == 3:
			var out store.NameOwner
			if err := c.do(http.MethodPut, "/api/v1/owners/"+url.PathEscape(pos[1]), nil,
				jsonBody(server.SetOwnerRequest{Owner: pos[2]}), &out); err != nil {
				return fail(err)
			}
			fmt.Fprintf(stdout, "%s is now owned by %s\n", out.Name, out.Owner)
		default:
			return usageErr("owner list | owner set NAME OWNER")
		}
	case "sync":
		if len(pos) != 1 {
			return usageErr("sync SOURCE")
		}
		if err := c.do(http.MethodPost, "/api/v1/sources/"+url.PathEscape(pos[0])+"/sync", nil, nil, nil); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "sync of %s started; see garner status\n", pos[0])
	case "approve":
		if len(pos) != 1 {
			return usageErr("approve PACKAGE_ID")
		}
		var out []server.PackageJSON
		if err := c.do(http.MethodPost, "/api/v1/packages/"+url.PathEscape(pos[0])+"/approve", nil, nil, &out); err != nil {
			return fail(err)
		}
		print(out, func() { printPackages(stdout, out) })
	case "conflicts":
		if sub != "list" && sub != "" {
			return usageErr("conflicts list [--all]")
		}
		q := url.Values{}
		if *all {
			q.Set("all", "1")
		}
		var out []store.Conflict
		if err := c.do(http.MethodGet, "/api/v1/conflicts", q, nil, &out); err != nil {
			return fail(err)
		}
		print(out, func() {
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "CREATED\tKIND\tPACKAGE\tDETAIL\tRESOLVED")
			for _, x := range out {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", x.CreatedAt, x.Kind, x.PackageRef, x.Detail, dash(x.ResolvedAt))
			}
			tw.Flush()
		})
	case "packages":
		if sub != "list" && sub != "" {
			return usageErr("packages list [--name N] [--distribution D] [--all]")
		}
		q := url.Values{}
		if *name != "" {
			q.Set("name", *name)
		}
		if *dist != "" {
			q.Set("distribution", *dist)
		}
		if *all {
			q.Set("all", "1")
		}
		var out []server.PackageJSON
		if err := c.do(http.MethodGet, "/api/v1/packages", q, nil, &out); err != nil {
			return fail(err)
		}
		print(out, func() { printPackages(stdout, out) })
	case "snapshot":
		switch {
		case sub == "list":
			var out []store.Snapshot
			if err := c.do(http.MethodGet, "/api/v1/snapshots", nil, nil, &out); err != nil {
				return fail(err)
			}
			print(out, func() {
				tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tCREATED\tCONFIG\tCURRENT")
				for _, s := range out {
					cur := ""
					if s.IsCurrent {
						cur = "*"
					}
					fmt.Fprintf(tw, "%s\t%s\t%.12s\t%s\n", s.ID, s.CreatedAt, s.ConfigHash, cur)
				}
				tw.Flush()
			})
		case sub == "rollback" && len(pos) == 2:
			var out store.Snapshot
			if err := c.do(http.MethodPost, "/api/v1/snapshots/"+url.PathEscape(pos[1])+"/rollback", nil, nil, &out); err != nil {
				return fail(err)
			}
			fmt.Fprintf(stdout, "published snapshot %s with the packages of %s\n", out.ID, pos[1])
		default:
			return usageErr("snapshot list | snapshot rollback ID")
		}
	case "publish":
		var out store.Snapshot
		if err := c.do(http.MethodPost, "/api/v1/publish", nil, nil, &out); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "published snapshot %s\n", out.ID)
	case "gc":
		var out map[string]any
		if err := c.do(http.MethodPost, "/api/v1/gc", nil, nil, &out); err != nil {
			return fail(err)
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	case "reload":
		var out map[string]string
		if err := c.do(http.MethodPost, "/api/v1/reload", nil, nil, &out); err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "reloaded; configuration %.12s\n", out["config_hash"])
	case "status":
		var st server.Status
		if err := c.do(http.MethodGet, "/api/v1/status", nil, nil, &st); err != nil {
			return fail(err)
		}
		print(st, func() {
			fmt.Fprintf(stdout, "version:       %s\nconfiguration: %.12s %s\nsnapshot:      %s (%s)\nconflicts:     %d open\nheld:          %d packages\n",
				st.Version, st.ConfigHash, st.ConfigCommit, dash(st.CurrentSnapshot), dash(st.PublishedAt),
				st.OpenConflicts, st.HeldPackages)
			if len(st.Sources) > 0 {
				tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "\nSOURCE\tLAST RUN\tLAST SUCCESS\tERROR")
				for _, s := range st.Sources {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, dash(s.LastRun), dash(s.LastSuccess), dash(s.LastError))
				}
				tw.Flush()
			}
		})
	case "token":
		switch sub {
		case "create":
			var out server.CreateTokenResponse
			if err := c.do(http.MethodPost, "/api/v1/tokens", nil, jsonBody(server.CreateTokenRequest{
				Name: *name, Owner: *owner, Scopes: splitList(*scope)}), &out); err != nil {
				return fail(err)
			}
			fmt.Fprintf(stdout, "token %s (owner %s, scopes %s), shown once:\n  GARNER_TOKEN=%s:%s\n",
				out.Token.Name, out.Token.Owner, strings.Join(out.Token.Scopes, ","), out.Token.Name, out.Secret)
		case "list":
			var out []*store.Token
			if err := c.do(http.MethodGet, "/api/v1/tokens", nil, nil, &out); err != nil {
				return fail(err)
			}
			printTokens(stdout, out)
		case "revoke":
			if len(pos) != 2 {
				return usageErr("token revoke NAME")
			}
			if err := c.do(http.MethodDelete, "/api/v1/tokens/"+url.PathEscape(pos[1]), nil, nil, nil); err != nil {
				return fail(err)
			}
			fmt.Fprintf(stdout, "revoked %s\n", pos[1])
		default:
			return usageErr("token create|list|revoke")
		}
	}
	return 0
}

func printPackages(w io.Writer, pkgs []server.PackageJSON) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFORMAT\tNAME\tVERSION\tARCH\tDISTRIBUTION\tSTATE\tORIGIN")
	for _, p := range pkgs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Format, p.Name, p.Version, p.Arch,
			p.Distribution, p.State, p.OriginRef)
	}
	tw.Flush()
}
