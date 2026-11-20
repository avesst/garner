package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = "server:\n  public_url: https://repo.example.com\n"

func TestDesignExampleIsValid(t *testing.T) {
	cfg, err := Load("testdata/design-example.yaml")
	if err != nil {
		t.Fatalf("design example rejected: %v", err)
	}
	if got := len(cfg.Sources); got != 2 {
		t.Errorf("sources = %d, want 2", got)
	}
	st := cfg.Settings(cfg.Sources[1])
	if st.PollInterval != 30*time.Minute || st.Retention != 5 || st.Verify != "opportunistic" {
		t.Errorf("platform-group settings = %+v", st)
	}
	if cfg.Forges[0].MaxConcurrent != defaultMaxConcurrent {
		t.Errorf("default max_concurrent not applied: %d", cfg.Forges[0].MaxConcurrent)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":8080" || cfg.Server.ClientName != "garner" {
		t.Errorf("server defaults = %+v", cfg.Server)
	}
	if cfg.Storage.MaxPackageSize != 2<<30 {
		t.Errorf("max_package_size = %d, want 2 GiB", cfg.Storage.MaxPackageSize)
	}
	if cfg.Storage.GCGrace != 10*time.Minute || cfg.Storage.IndexHistory != time.Hour {
		t.Errorf("storage defaults = %+v", cfg.Storage)
	}
	if p, _ := cfg.SQLitePath(); p != "/var/lib/garner/garner.db" {
		t.Errorf("sqlite path = %q", p)
	}
	if len(cfg.Hash) != 64 {
		t.Errorf("hash = %q", cfg.Hash)
	}
}

func TestPartialSectionKeepsOtherDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal + "storage:\n  gc_grace: 5m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.GCGrace != 5*time.Minute || cfg.Storage.Path != "/var/lib/garner" {
		t.Errorf("storage = %+v", cfg.Storage)
	}
}

func TestInvalid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string // substrings that must appear in the error
	}{
		{"empty file", "", []string{"server.public_url: is required"}},
		{"unknown key", minimal + "sever:\n  listen: x\n", []string{"field sever not found"}},
		{"two documents", minimal + "---\n" + minimal, []string{"more than one YAML document"}},
		{"bad duration", minimal + "storage:\n  gc_grace: soon\n", []string{"soon"}},
		{"bad size", minimal + "storage:\n  max_package_size: lots\n", []string{`invalid size "lots"`}},
		{"zero size", minimal + "storage:\n  max_package_size: 0\n", []string{"storage.max_package_size: must be greater than zero"}},
		{"old size key", minimal + "  max_upload_size: 2GiB\n", []string{"field max_upload_size not found"}},
		{"postgres", minimal + "database:\n  url: postgres://db/garner\n", []string{"PostgreSQL is not supported yet"}},
		{"relative sqlite", minimal + "database:\n  url: sqlite://garner.db\n", []string{"three slashes"}},
		{"bad proxy", minimal + "  trusted_proxies: [\"10.0.0.0/33\"]\n", []string{"server.trusted_proxies[0]"}},
		{"arch all", minimal + "deb:\n  architectures: [amd64, all]\n", []string{`"all" is implicit`}},
		{"duplicate suite", minimal + "deb:\n  suites: [stable, stable]\n", []string{"listed twice"}},
		{"bad enum", minimal + "defaults:\n  verify: sometimes\n", []string{"defaults.verify"}},
		{"forge without url", minimal + "forges:\n  - {name: f, type: forgejo}\n", []string{"url: is required"}},
		{"forge over http", minimal + "forges:\n  - {name: f, type: forgejo, url: \"http://git\"}\n", []string{"https URL"}},
		{"two tokens", minimal + "forges:\n  - {name: gh, type: github, token_env: A, token_file: /b}\n", []string{"not both"}},
		{"unknown forge", minimal + "sources:\n  - {name: s, forge: nope, repo: a/b}\n", []string{`unknown forge "nope"`}},
		{"repo and org", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, repo: a/b, org: a}\n", []string{"exactly one of repo, org or group"}},
		{"group on github", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, group: a}\n", []string{"use org"}},
		{"unknown suite", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - name: s\n    forge: gh\n    repo: a/b\n    assets:\n      - {match: \"*\", suite: nobel}\n", []string{`unknown suite "nobel" (deb.suites: stable)`}},
		{"bad glob", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, repo: a/b, owns: [\"[a-\"]}\n", []string{"sources[0].owns[0]"}},
		{"bad checksums glob", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, repo: a/b, checksums: [\"ok\", \"[x\"]}\n", []string{"sources[0].checksums[1]"}},
		{"source override", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, repo: a/b, retention: 0}\n", []string{"sources[0].retention"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestAllProblemsReportedAtOnce(t *testing.T) {
	_, err := Parse([]byte("log:\n  format: xml\n  level: loud\n"))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v", err)
	}
	if len(ve.Problems) != 3 { // public_url, format, level
		t.Errorf("problems = %d, want 3:\n%v", len(ve.Problems), err)
	}
}

func TestOwnsOverlap(t *testing.T) {
	y := minimal + `forges:
  - {name: gh, type: github}
sources:
  - {name: tool, forge: gh, repo: a/tool, owns: ["tool-*"]}
  - {name: cli, forge: gh, repo: a/cli, owns: ["*-cli"]}
  - {name: other, forge: gh, repo: a/other, owns: ["other"]}
`
	_, err := Parse([]byte(y))
	if err == nil {
		t.Fatal("overlapping owns: accepted")
	}
	want := `sources[1].owns: "*-cli" overlaps "tool-*" in source "tool"; both match "tool-cli"`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v\nwant it to contain %s", err, want)
	}
	if strings.Contains(err.Error(), `"other"`) {
		t.Errorf("non-overlapping source reported: %v", err)
	}
}

func TestByteSize(t *testing.T) {
	tests := map[string]ByteSize{
		"0": 0, "1024": 1024, "2GiB": 2 << 30, "500MB": 500e6, "64 KiB": 64 << 10, "10B": 10,
	}
	for in, want := range tests {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "1.5GiB", "2XB", "99999999999TiB"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) succeeded, want error", in)
		}
	}
}

func TestHolderKeepsConfigOnInvalidReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garner.yaml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(minimal)
	h, err := NewHolder(path)
	if err != nil {
		t.Fatal(err)
	}
	first := h.Current()

	write("log:\n  format: xml\n")
	cur, err := h.Reload()
	if err == nil {
		t.Fatal("invalid reload succeeded")
	}
	if cur != first || h.Current() != first {
		t.Error("invalid reload replaced the active configuration")
	}

	write(minimal + "log:\n  level: debug\n")
	if _, err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	if h.Current().Log.Level != "debug" || h.Current().Hash == first.Hash {
		t.Error("valid reload not applied")
	}
}
