package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/avesst/garner/internal/glob"
)

var (
	// Names used in URLs, file names and metric labels.
	reName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// Client file prefix: becomes <name>.sources, <name>.repo, <name>.gpg.
	reClientName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	// Debian suite and rpm release names.
	reDistribution = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	reDebArch      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	reRPMArch      = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)
)

// problems collects validation messages, each prefixed with the key path.
type problems []string

func (p *problems) add(path, format string, args ...any) {
	*p = append(*p, path+": "+fmt.Sprintf(format, args...))
}

// Validate checks the configuration and reports every problem at once.
// It works offline: files referenced by *_file keys are not opened, so CI can
// validate a file whose secrets only exist in production.
func (c *Config) Validate() error {
	var p problems
	c.validateServer(&p)
	c.validateLogAndMetrics(&p)
	c.validateStorage(&p)
	for i, k := range c.Signing.Keys {
		if k.KeyFile == "" {
			p.add(fmt.Sprintf("signing.keys[%d].key_file", i), "is required")
		}
	}
	validateList(&p, "deb.architectures", c.Deb.Architectures, reDebArch, "all")
	validateList(&p, "deb.suites", c.Deb.Suites, reDistribution, "")
	validateList(&p, "rpm.architectures", c.RPM.Architectures, reRPMArch, "noarch")
	validateList(&p, "rpm.releases", c.RPM.Releases, reDistribution, "")
	validateSettings(&p, "defaults", c.Defaults.Retention, c.Defaults.PollInterval,
		c.Defaults.Verify, c.Defaults.Approval)
	forges := c.validateForges(&p)
	c.validateSources(&p, forges)
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

func (c *Config) validateServer(p *problems) {
	s := c.Server
	validateListen(p, "server.listen", s.Listen, false)
	if s.PublicURL == "" {
		p.add("server.public_url", "is required")
	} else if u, err := url.Parse(s.PublicURL); err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" {
		p.add("server.public_url", "must be an absolute http or https URL without query or fragment, got %q", s.PublicURL)
	}
	if !reClientName.MatchString(s.ClientName) {
		p.add("server.client_name", "must match %s, got %q", reClientName, s.ClientName)
	}
	for i, tp := range s.TrustedProxies {
		if _, err := parseProxy(tp); err != nil {
			p.add(fmt.Sprintf("server.trusted_proxies[%d]", i), "must be an IP address or CIDR prefix, got %q", tp)
		}
	}
	if s.MaxUploadSize <= 0 {
		p.add("server.max_upload_size", "must be greater than zero")
	}
}

// TrustedProxyPrefixes returns server.trusted_proxies as prefixes. A bare
// address becomes a single-address prefix. Call it only on a validated
// configuration.
func (c *Config) TrustedProxyPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(c.Server.TrustedProxies))
	for _, s := range c.Server.TrustedProxies {
		if pfx, err := parseProxy(s); err == nil {
			out = append(out, pfx)
		}
	}
	return out
}

func parseProxy(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		pfx, err := netip.ParsePrefix(s)
		return pfx.Masked(), err
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func validateListen(p *problems, path, addr string, emptyOK bool) {
	if addr == "" {
		if !emptyOK {
			p.add(path, "is required")
		}
		return
	}
	if _, port, err := net.SplitHostPort(addr); err != nil || port == "" {
		p.add(path, "must be host:port or :port, got %q", addr)
	}
}

func (c *Config) validateLogAndMetrics(p *problems) {
	validateListen(p, "metrics.listen", c.Metrics.Listen, true)
	if !slices.Contains([]string{"json", "text"}, c.Log.Format) {
		p.add("log.format", "must be json or text, got %q", c.Log.Format)
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		p.add("log.level", "must be debug, info, warn or error, got %q", c.Log.Level)
	}
}

func (c *Config) validateStorage(p *problems) {
	if _, err := c.SQLitePath(); err != nil {
		p.add("database.url", "%v", err)
	}
	if !filepath.IsAbs(c.Storage.Path) {
		p.add("storage.path", "must be an absolute path, got %q", c.Storage.Path)
	}
	if c.Storage.GCGrace < 0 {
		p.add("storage.gc_grace", "must not be negative")
	}
	if c.Storage.IndexHistory < 0 {
		p.add("storage.index_history", "must not be negative")
	}
	if c.Storage.SnapshotRetention < 2 {
		p.add("storage.snapshot_retention", "must be at least 2, so the previous snapshot survives a publish")
	}
}

// SQLitePath returns the database file path from database.url.
func (c *Config) SQLitePath() (string, error) {
	u := c.Database.URL
	switch {
	case strings.HasPrefix(u, "postgres://"), strings.HasPrefix(u, "postgresql://"):
		return "", fmt.Errorf("PostgreSQL is not supported yet; use sqlite:///path/to/garner.db")
	case !strings.HasPrefix(u, "sqlite://"):
		return "", fmt.Errorf("must be sqlite:///absolute/path, got %q", u)
	}
	path := strings.TrimPrefix(u, "sqlite://")
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("must be sqlite:///absolute/path (three slashes), got %q", u)
	}
	return path, nil
}

// validateList checks a non-empty list of unique names. forbidden is a
// name with special meaning that may not be configured explicitly.
func validateList(p *problems, path string, items []string, re *regexp.Regexp, forbidden string) {
	if len(items) == 0 {
		p.add(path, "must list at least one entry")
	}
	seen := map[string]bool{}
	for i, it := range items {
		ip := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case forbidden != "" && it == forbidden:
			p.add(ip, "%q is implicit and must not be listed", it)
		case !re.MatchString(it):
			p.add(ip, "must match %s, got %q", re, it)
		case seen[it]:
			p.add(ip, "%q is listed twice", it)
		}
		seen[it] = true
	}
}

func validateSettings(p *problems, path string, retention int, poll time.Duration, verify, approval string) {
	if retention < 1 {
		p.add(path+".retention", "must be at least 1, got %d", retention)
	}
	if poll < time.Minute {
		p.add(path+".poll_interval", "must be at least 1m, got %s", poll)
	}
	if verify != "opportunistic" && verify != "required" {
		p.add(path+".verify", "must be opportunistic or required, got %q", verify)
	}
	if approval != "auto" && approval != "manual" {
		p.add(path+".approval", "must be auto or manual, got %q", approval)
	}
}

// forgeTypes maps each accepted type to its canonical name; gitea is an
// alias of forgejo.
var forgeTypes = map[string]string{
	"github": "github", "forgejo": "forgejo", "gitea": "forgejo", "gitlab": "gitlab",
}

// validateForges checks the forges and returns their canonical types by name.
func (c *Config) validateForges(p *problems) map[string]string {
	types := map[string]string{}
	for i, f := range c.Forges {
		fp := fmt.Sprintf("forges[%d]", i)
		if !reName.MatchString(f.Name) {
			p.add(fp+".name", "must match %s, got %q", reName, f.Name)
		} else if _, dup := types[f.Name]; dup {
			p.add(fp+".name", "%q is used by another forge", f.Name)
		}
		typ, ok := forgeTypes[f.Type]
		if !ok {
			p.add(fp+".type", "must be github, forgejo, gitea or gitlab, got %q", f.Type)
		}
		types[f.Name] = typ
		switch {
		case f.URL == "" && typ != "github":
			p.add(fp+".url", "is required for type %s", f.Type)
		case f.URL != "":
			// Tokens are sent to this URL, so it must use TLS (design section 7).
			if u, err := url.Parse(f.URL); err != nil || u.Scheme != "https" || u.Host == "" {
				p.add(fp+".url", "must be an absolute https URL, got %q", f.URL)
			}
		}
		if f.TokenEnv != "" && f.TokenFile != "" {
			p.add(fp, "set token_env or token_file, not both")
		}
		if f.MaxConcurrent < 1 {
			p.add(fp+".max_concurrent", "must be at least 1, got %d", f.MaxConcurrent)
		}
		if f.Proxy != "" {
			if u, err := url.Parse(f.Proxy); err != nil || u.Host == "" ||
				!slices.Contains([]string{"http", "https", "socks5"}, u.Scheme) {
				p.add(fp+".proxy", "must be an http, https or socks5 URL, got %q", f.Proxy)
			}
		}
	}
	return types
}

func (c *Config) validateSources(p *problems, forges map[string]string) {
	names := map[string]bool{}
	for i, s := range c.Sources {
		sp := fmt.Sprintf("sources[%d]", i)
		if !reName.MatchString(s.Name) {
			p.add(sp+".name", "must match %s, got %q", reName, s.Name)
		} else if names[s.Name] {
			p.add(sp+".name", "%q is used by another source", s.Name)
		}
		names[s.Name] = true

		typ, ok := forges[s.Forge]
		if !ok {
			p.add(sp+".forge", "unknown forge %q", s.Forge)
		}
		c.validateSourceTarget(p, sp, s, typ)
		validateGlobs(p, sp+".owns", s.Owns)
		validateGlobs(p, sp+".include", s.Include)
		validateGlobs(p, sp+".exclude", s.Exclude)
		validateGlobs(p, sp+".checksums", s.Checksums)
		c.validateAssets(p, sp, s.Assets)

		st := c.Settings(s)
		validateSettings(p, sp, st.Retention, st.PollInterval, st.Verify, st.Approval)
	}
	c.validateOwnership(p)
}

func (c *Config) validateSourceTarget(p *problems, sp string, s Source, typ string) {
	set := 0
	for _, v := range []string{s.Repo, s.Org, s.Group} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		p.add(sp, "set exactly one of repo, org or group")
		return
	}
	switch {
	case s.Repo != "":
		parts := strings.Split(s.Repo, "/")
		if len(parts) < 2 || slices.Contains(parts, "") {
			p.add(sp+".repo", "must be owner/name, got %q", s.Repo)
		}
		if len(s.Include) > 0 {
			p.add(sp+".include", "only applies to org and group sources")
		}
	case s.Org != "" && typ == "gitlab":
		p.add(sp+".org", "GitLab sources use group, not org")
	case s.Group != "" && typ != "" && typ != "gitlab":
		p.add(sp+".group", "only GitLab sources use group; use org")
	}
	if s.Recursive != nil && (s.Group == "" || typ != "gitlab") {
		p.add(sp+".recursive", "only applies to GitLab group sources")
	}
}

func validateGlobs(p *problems, path string, globs []string) {
	for i, g := range globs {
		if err := glob.Validate(g); err != nil {
			p.add(fmt.Sprintf("%s[%d]", path, i), "%v", err)
		}
	}
}

func (c *Config) validateAssets(p *problems, sp string, rules []AssetRule) {
	for j, r := range rules {
		rp := fmt.Sprintf("%s.assets[%d]", sp, j)
		if r.Match == "" {
			p.add(rp+".match", "is required")
		} else if err := glob.Validate(r.Match); err != nil {
			p.add(rp+".match", "%v", err)
		}
		if r.Suite == "" && r.Release == "" {
			p.add(rp, "set suite, release or both")
		}
		if r.Suite != "" && !slices.Contains(c.Deb.Suites, r.Suite) {
			p.add(rp+".suite", "unknown suite %q (deb.suites: %s)", r.Suite, strings.Join(c.Deb.Suites, ", "))
		}
		if r.Release != "" && !slices.Contains(c.RPM.Releases, r.Release) {
			p.add(rp+".release", "unknown release %q (rpm.releases: %s)", r.Release, strings.Join(c.RPM.Releases, ", "))
		}
	}
}

// validateOwnership rejects owns: globs of different sources that can match
// the same package name (design section 8).
func (c *Config) validateOwnership(p *problems) {
	for i := range c.Sources {
		for j := i + 1; j < len(c.Sources); j++ {
			a, b := c.Sources[i], c.Sources[j]
			for _, ga := range a.Owns {
				for _, gb := range b.Owns {
					ex, ok, err := glob.Overlap(ga, gb)
					if err != nil || !ok {
						continue // invalid globs are reported separately
					}
					p.add(fmt.Sprintf("sources[%d].owns", j),
						"%q overlaps %q in source %q; both match %q",
						gb, ga, a.Name, ex)
				}
			}
		}
	}
}
