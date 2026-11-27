package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/avesst/garner/internal/app"
	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/db"
	"github.com/avesst/garner/internal/signing"
	"github.com/avesst/garner/internal/store"
)

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("garner "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func runServe(args []string, stderr io.Writer) int {
	fs := newFlags("serve", stderr)
	cfgPath := fs.String("config", config.DefaultPath, "configuration file")
	if pos, err := parse(fs, args); err != nil || len(pos) != 0 {
		return 2
	}
	ctx := context.Background()
	var a *app.App
	signer := func() signing.Signer { return a.Signer() }
	a, err := app.Open(ctx, *cfgPath, versionString(), formats(signer))
	if err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	defer a.Close()
	if err := attachLive(a); err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	if err := startSync(a); err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	if err := a.Run(ctx, backgroundLoops(a)...); err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	return 0
}

// runInit prepares a new installation. It is safe to run again: it never
// replaces a key or a token.
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("init", stderr)
	cfgPath := fs.String("config", config.DefaultPath, "configuration file")
	if pos, err := parse(fs, args); err != nil || len(pos) != 0 {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", *cfgPath, err)
		return 1
	}
	if err := os.MkdirAll(cfg.Storage.Path, 0o755); err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	if len(cfg.Signing.Keys) == 0 {
		path := app.DefaultKeyPath(cfg)
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintf(stdout, "signing key: %s exists, kept\n", path)
		} else {
			fmt.Fprintf(stdout, "generating an RSA 4096 signing key (this takes a few seconds)...\n")
			fpr, err := signing.Generate(path, cfg.Server.ClientName+" repository", "")
			if err != nil {
				fmt.Fprintf(stderr, "garner: generate key: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "signing key: %s, fingerprint %s\n", path, fpr)
		}
	} else {
		if _, err := signing.Load(app.KeySources(cfg)); err != nil {
			fmt.Fprintf(stderr, "garner: signing.keys: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "signing key: using %s\n", cfg.Signing.Keys[0].KeyFile)
	}

	d, err := openDB(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	defer d.Close()
	st := store.New(d.DB)
	tokens, err := st.Tokens(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	if len(tokens) > 0 {
		fmt.Fprintf(stdout, "tokens: %d exist; no admin token created\n", len(tokens))
		return 0
	}
	secret, _, err := auth.CreateToken(context.Background(), &st.Queries, "admin", "admin", auth.AllScopes)
	if err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "admin token (shown once; scopes %s):\n  GARNER_TOKEN=admin:%s\n",
		strings.Join(auth.AllScopes, ","), secret)
	return 0
}

func openDB(cfg *config.Config) (*db.DB, error) {
	path, err := cfg.SQLitePath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	d, err := db.Open(context.Background(), path)
	if errors.Is(err, db.ErrLocked) {
		return nil, fmt.Errorf("%w; while garner serve runs, use --url to go through the API", err)
	}
	return d, err
}

// runToken manages tokens locally, or through the API with --url.
func runToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: garner token create --name N --owner O --scopes S[,S] | list | revoke NAME  [--url URL | --config FILE]")
		return 2
	}
	sub := args[0]
	fs := newFlags("token "+sub, stderr)
	cfgPath := fs.String("config", config.DefaultPath, "configuration file (local mode)")
	url := fs.String("url", os.Getenv("GARNER_URL"), "garner URL (API mode)")
	name, owner, scopes := new(string), new(string), new(string)
	if sub == "create" {
		fs.StringVar(name, "name", "", "token name")
		fs.StringVar(owner, "owner", "", "owner label")
		fs.StringVar(scopes, "scopes", "", "comma-separated scopes: upload, publish, admin")
	}
	pos, err := parse(fs, args[1:])
	if err != nil {
		return 2
	}
	if *url != "" {
		var apiArgs []string
		if sub == "create" {
			apiArgs = []string{"--name", *name, "--owner", *owner, "--scopes", *scopes}
		}
		apiArgs = append(apiArgs, sub)
		return runAPIWith(*url, "token", append(apiArgs, pos...), stdout, stderr)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", *cfgPath, err)
		return 1
	}
	d, err := openDB(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "garner: %v\n", err)
		return 1
	}
	defer d.Close()
	st := store.New(d.DB)
	ctx := context.Background()
	switch sub {
	case "create":
		secret, t, err := auth.CreateToken(ctx, &st.Queries, *name, *owner, splitList(*scopes))
		if err != nil {
			fmt.Fprintf(stderr, "garner: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "token %s (owner %s, scopes %s), shown once:\n  GARNER_TOKEN=%s:%s\n",
			t.Name, t.Owner, strings.Join(t.Scopes, ","), t.Name, secret)
	case "list":
		ts, err := st.Tokens(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "garner: %v\n", err)
			return 1
		}
		printTokens(stdout, ts)
	case "revoke":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "Usage: garner token revoke NAME")
			return 2
		}
		if err := st.RevokeToken(ctx, pos[0]); err != nil {
			fmt.Fprintf(stderr, "garner: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "revoked %s\n", pos[0])
	default:
		fmt.Fprintf(stderr, "garner token: unknown subcommand %q\n", sub)
		return 2
	}
	return 0
}

func printTokens(w io.Writer, ts []*store.Token) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOWNER\tSCOPES\tCREATED\tLAST USED\tREVOKED")
	for _, t := range ts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, t.Owner, strings.Join(t.Scopes, ","),
			t.CreatedAt, dash(t.LastUsedAt), dash(t.RevokedAt))
	}
	tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parse parses flags anywhere among the arguments, as users expect from
// "garner upload FILE --suite noble"; the standard flag package stops at
// the first positional argument. A "--" ends flag parsing.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if len(args) > 0 && len(rest) < len(args) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}
