package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// serverOnly has no format section. minimal adds deb; it ends inside the
// server section, so cases can append indented server keys.
const (
	serverOnly = "server:\n  public_url: https://repo.example.com\n"
	minimal    = "deb: {}\n" + serverOnly
)

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

// A format is enabled by its section being present. A bare section gets
// the defaults, and a partial one keeps the defaults of keys it leaves out.
func TestFormatSections(t *testing.T) {
	tests := []struct {
		name, yaml string
		formats    []string
		check      func(*Config) bool
	}{
		{"bare deb", serverOnly + "deb:\n", []string{"deb"}, func(c *Config) bool {
			return slices.Equal(c.Deb.Suites, []string{"stable"}) && slices.Equal(c.Deb.Architectures, []string{"amd64"})
		}},
		{"partial rpm", serverOnly + "rpm:\n  releases: [el10]\n", []string{"rpm"}, func(c *Config) bool {
			return slices.Equal(c.RPM.Releases, []string{"el10"}) && slices.Equal(c.RPM.Architectures, []string{"x86_64"})
		}},
		{"both", serverOnly + "deb: {}\nrpm: {}\n", []string{"deb", "rpm"}, func(c *Config) bool { return true }},
		{"bare oci", serverOnly + "oci:\n", []string{"oci"}, func(c *Config) bool {
			return c.OCI.Retention == 10 && c.OCI.UploadExpiry == 24*time.Hour
		}},
		{"through a merge key", serverOnly + "<<: {deb: {suites: [main]}}\n", []string{"deb"}, func(c *Config) bool {
			return slices.Equal(c.Deb.Suites, []string{"main"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Formats(); !slices.Equal(got, tc.formats) {
				t.Errorf("formats = %v, want %v", got, tc.formats)
			}
			if !tc.check(cfg) {
				t.Errorf("deb = %+v, rpm = %+v", cfg.Deb, cfg.RPM)
			}
		})
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
		{"arch all", serverOnly + "deb:\n  architectures: [amd64, all]\n", []string{`"all" is implicit`}},
		{"i686 without x86_64", serverOnly + "rpm:\n  architectures: [i686]\n", []string{"x86_64 must be listed too"}},
		{"duplicate suite", serverOnly + "deb:\n  suites: [stable, stable]\n", []string{"listed twice"}},
		{"bad enum", minimal + "defaults:\n  verify: sometimes\n", []string{"defaults.verify"}},
		{"negative min_release_age", minimal + "defaults:\n  min_release_age: -1h\n", []string{"defaults.min_release_age: must not be negative"}},
		{"min_release_age in days", minimal + "defaults:\n  min_release_age: 14d\n", []string{"14d"}},
		{"forge without url", minimal + "forges:\n  - {name: f, type: forgejo}\n", []string{"url: is required"}},
		{"forge over http", minimal + "forges:\n  - {name: f, type: forgejo, url: \"http://git\"}\n", []string{"https URL"}},
		{"two tokens", minimal + "forges:\n  - {name: gh, type: github, token_env: A, token_file: /b}\n", []string{"not both"}},
		{"unknown forge", minimal + "sources:\n  - {name: s, forge: nope, repo: a/b}\n", []string{`unknown forge "nope"`}},
		{"repo and org", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, repo: a/b, org: a}\n", []string{"exactly one of repo, org or group"}},
		{"group on github", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, group: a}\n", []string{"use org"}},
		{"unknown suite", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - name: s\n    forge: gh\n    repo: a/b\n    assets:\n      - {match: \"*\", suite: nobel}\n", []string{`unknown suite "nobel" (deb.suites: stable)`}},
		{"bad glob", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, repo: a/b, owns: [\"[a-\"]}\n", []string{"sources[0].owns[0]"}},
		{"bad checksums glob", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, repo: a/b, checksums: [\"ok\", \"[x\"]}\n", []string{"sources[0].checksums[1]"}},
		{"no format", serverOnly, []string{"no package format is configured"}},
		{"suite without deb", serverOnly + "rpm: {}\nforges:\n  - {name: gh, type: github}\nsources:\n  - name: s\n    forge: gh\n    repo: a/b\n    assets:\n      - {match: \"*\", suite: stable}\n", []string{"sources[0].assets[0].suite: set, but deb is not configured"}},
		{"release without rpm", minimal + "forges:\n  - {name: gh, type: github}\nsources:\n  - name: s\n    forge: gh\n    repo: a/b\n    assets:\n      - {match: \"*\", release: stable}\n", []string{"sources[0].assets[0].release: set, but rpm is not configured"}},
		{"oci retention", serverOnly + "oci:\n  retention: -1\n", []string{"oci.retention: must not be negative"}},
		{"oci expiry", serverOnly + "oci:\n  upload_expiry: 10s\n", []string{"oci.upload_expiry"}},
		{"oci public_url with path", serverOnly + "oci:\n  public_url: https://reg.example/v2\n", []string{"oci.public_url"}},
		{"oci public_url not http", serverOnly + "oci:\n  public_url: reg.example\n", []string{"oci.public_url"}},
		{"oci public_url with credentials", serverOnly + "oci:\n  public_url: https://u:secret@reg.example\n", []string{"oci.public_url", "u:xxxxx@"}},
		{"public_url with credentials", "deb: {}\nserver:\n  public_url: https://u:secret@repo.example\n", []string{"server.public_url", "u:xxxxx@"}},
		{"registry over http", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"http://reg\"}\n", []string{"registries[0].url"}},
		{"registry with path", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg/v2\"}\n", []string{"without a path"}},
		{"username without token", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg\", username: u}\n", []string{"needs token_env or token_file"}},
		{"bad auth host", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg\", auth_hosts: [\"https://x\"]}\n", []string{"auth_hosts[0]"}},
		{"registry source without oci", minimal + "registries:\n  - {name: r, url: \"https://reg\"}\nsources:\n  - {name: s, registry: r, image: a/b, tags: [\"*\"]}\n", []string{"need the oci section"}},
		{"registry source without tags", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg\"}\nsources:\n  - {name: s, registry: r, image: a/b}\n", []string{"sources[0].tags"}},
		{"registry source with assets", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg\"}\nsources:\n  - {name: s, registry: r, image: a/b, tags: [x], repo: a/b}\n", []string{"only apply to forge sources"}},
		{"bad image", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg\"}\nsources:\n  - {name: s, registry: r, image: A/B, tags: [x]}\n", []string{"sources[0].image"}},
		{"reserved image name", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg\"}\nsources:\n  - {name: s, registry: r, image: a/b, as: a/manifests, tags: [x]}\n", []string{"reserved component"}},
		{"unknown registry", serverOnly + "oci: {}\nsources:\n  - {name: s, registry: r, image: a/b, tags: [x]}\n", []string{`unknown registry "r"`}},
		{"forge and registry", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg\"}\nforges:\n  - {name: gh, type: github}\nsources:\n  - {name: s, forge: gh, registry: r, image: a/b, tags: [x]}\n", []string{"forge or registry, not both"}},
		{"same local image name", serverOnly + "oci: {}\nregistries:\n  - {name: r, url: \"https://reg\"}\nsources:\n  - {name: a, registry: r, image: x/tool, as: tool, tags: [x]}\n  - {name: b, registry: r, image: y/tool, as: tool, tags: [x]}\n", []string{`sources[1]: serves its images as "tool", as source "a" does`}},
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

func TestOCIPublicURLWithTrailingSlashIsValid(t *testing.T) {
	cfg, err := Parse([]byte(serverOnly + "oci:\n  public_url: https://reg.example/\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OCI.PublicURL != "https://reg.example/" {
		t.Errorf("oci.public_url = %q", cfg.OCI.PublicURL)
	}
}

func TestAllProblemsReportedAtOnce(t *testing.T) {
	_, err := Parse([]byte("deb: {}\nlog:\n  format: xml\n  level: loud\n"))
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

func TestMinReleaseAgePrecedence(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `defaults:
  min_release_age: 72h
forges:
  - {name: gh, type: github}
sources:
  - {name: a, forge: gh, repo: o/a}
  - {name: b, forge: gh, repo: o/b, min_release_age: 0s}
  - {name: c, forge: gh, repo: o/c, min_release_age: 336h}
`))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []time.Duration{72 * time.Hour, 0, 336 * time.Hour} {
		if got := cfg.Settings(cfg.Sources[i]).MinReleaseAge; got != want {
			t.Errorf("source %s: min_release_age = %s, want %s", cfg.Sources[i].Name, got, want)
		}
	}
	if d := Default().Defaults.MinReleaseAge; d != 0 {
		t.Errorf("default min_release_age = %s, want 0s", d)
	}
}

// Forge and registry sources reserve names in different namespaces, so
// the same glob in both is no conflict; two registry sources still clash.
func TestOwnsOverlapByKind(t *testing.T) {
	base := minimal + `oci: {}
forges:
  - {name: gh, type: github}
registries:
  - {name: r, url: "https://reg"}
sources:
  - {name: tool, forge: gh, repo: a/tool, owns: ["tool"]}
  - {name: image, registry: r, image: a/tool, tags: ["*"], owns: ["tool"]}
`
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatalf("forge and registry source owning tool: %v", err)
	}
	_, err := Parse([]byte(base + "  - {name: image2, registry: r, image: a/tool2, tags: [\"*\"], owns: [\"t*\"]}\n"))
	if err == nil || !strings.Contains(err.Error(), `overlaps "tool" in source "image"`) {
		t.Errorf("two registry sources owning tool: %v", err)
	}
}
