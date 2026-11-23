// Package config loads and validates garner's YAML configuration
// (design section 5).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultPath is used when no --config flag is given.
const DefaultPath = "/etc/garner/garner.yaml"

// Config is the whole configuration file.
type Config struct {
	Server   Server   `yaml:"server"`
	Metrics  Metrics  `yaml:"metrics"`
	Log      Log      `yaml:"log"`
	Database Database `yaml:"database"`
	Storage  Storage  `yaml:"storage"`
	Signing  Signing  `yaml:"signing"`
	// Deb and RPM are nil when the file has no such section: a format is
	// enabled by its section being present.
	Deb        *Deb       `yaml:"deb"`
	RPM        *RPM       `yaml:"rpm"`
	OCI        *OCI       `yaml:"oci"`
	Defaults   Defaults   `yaml:"defaults"`
	Forges     []Forge    `yaml:"forges"`
	Registries []Registry `yaml:"registries"`
	Sources    []Source   `yaml:"sources"`

	// Hash is the hex sha256 of the file the configuration was loaded from.
	Hash string `yaml:"-"`
}

type Server struct {
	Listen            string   `yaml:"listen"`
	PublicURL         string   `yaml:"public_url"`
	ClientName        string   `yaml:"client_name"`
	TrustedProxies    []string `yaml:"trusted_proxies"`
	AllowInsecureAuth bool     `yaml:"allow_insecure_auth"`
}

type Metrics struct {
	// Listen is empty to serve /metrics on Server.Listen.
	Listen string `yaml:"listen"`
}

type Log struct {
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
}

type Database struct {
	URL string `yaml:"url"`
}

type Storage struct {
	Path         string        `yaml:"path"`
	GCGrace      time.Duration `yaml:"gc_grace"`
	IndexHistory time.Duration `yaml:"index_history"`
	// SnapshotRetention is how many snapshots are kept, newest first.
	SnapshotRetention int `yaml:"snapshot_retention"`
	// MaxPackageSize limits an uploaded package and a synced asset alike.
	MaxPackageSize ByteSize `yaml:"max_package_size"`
}

type Signing struct {
	// Keys are all published; the first one signs.
	Keys []SigningKey `yaml:"keys"`
}

type SigningKey struct {
	KeyFile        string `yaml:"key_file"`
	PassphraseFile string `yaml:"passphrase_file"`
}

type Deb struct {
	Architectures []string `yaml:"architectures"`
	Suites        []string `yaml:"suites"`
}

type RPM struct {
	Architectures []string `yaml:"architectures"`
	Releases      []string `yaml:"releases"`
}

// OCI configures container images (design section 19).
type OCI struct {
	// PublicURL is the URL of the registry when it differs from
	// server.public_url, such as a hostname of its own. Empty uses
	// server.public_url.
	PublicURL string `yaml:"public_url"`
	// Retention is how many untagged manifests are kept per name, besides
	// tagged ones, for images pushed to garner; 0 keeps all.
	Retention int `yaml:"retention"`
	// UploadExpiry is how long pushed blobs that no manifest references
	// yet, and idle upload sessions, are kept.
	UploadExpiry time.Duration `yaml:"upload_expiry"`
}

// Defaults apply to every source that does not override them.
type Defaults struct {
	Retention    int           `yaml:"retention"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Verify       string        `yaml:"verify"`
	Approval     string        `yaml:"approval"`
	Prereleases  bool          `yaml:"prereleases"`
	// MinReleaseAge is how old a synced release must be before it is
	// processed; 0 turns the wait off.
	MinReleaseAge time.Duration `yaml:"min_release_age"`
}

type Forge struct {
	Name          string `yaml:"name"`
	Type          string `yaml:"type"`
	URL           string `yaml:"url"`
	TokenEnv      string `yaml:"token_env"`
	TokenFile     string `yaml:"token_file"`
	CAFile        string `yaml:"ca_file"`
	MaxConcurrent int    `yaml:"max_concurrent"`
	Proxy         string `yaml:"proxy"`
}

// Registry is an OCI registry that registry sources sync images from
// (design section 19).
type Registry struct {
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`
	Username  string `yaml:"username"`
	TokenEnv  string `yaml:"token_env"`
	TokenFile string `yaml:"token_file"`
	// AuthHosts are hosts besides the registry's own that may receive its
	// credentials as token endpoints (Docker Hub's auth.docker.io).
	AuthHosts     []string `yaml:"auth_hosts"`
	CAFile        string   `yaml:"ca_file"`
	MaxConcurrent int      `yaml:"max_concurrent"`
	Proxy         string   `yaml:"proxy"`
}

// Source is one sync origin: a single repository (Repo) or a whole
// organization (Org) or GitLab group (Group) of a forge, or an image
// (Image) of a registry.
type Source struct {
	Name    string      `yaml:"name"`
	Forge   string      `yaml:"forge"`
	Repo    string      `yaml:"repo"`
	Org     string      `yaml:"org"`
	Group   string      `yaml:"group"`
	Owns    []string    `yaml:"owns"`
	Assets  []AssetRule `yaml:"assets"`
	Include []string    `yaml:"include"`
	Exclude []string    `yaml:"exclude"`
	// Checksums are globs naming a release's checksum files; they replace
	// the built-in names (checksums.txt, SHA256SUMS, ...).
	Checksums []string `yaml:"checksums"`

	// Registry sources: the registry, the image path on it, globs of the
	// tags to sync and the local name (default Image).
	Registry string   `yaml:"registry"`
	Image    string   `yaml:"image"`
	Tags     []string `yaml:"tags"`
	As       string   `yaml:"as"`

	Archived  bool  `yaml:"archived"`
	Forks     bool  `yaml:"forks"`
	Recursive *bool `yaml:"recursive"`

	// Overrides of Defaults; nil means "use the default".
	PollInterval  *time.Duration `yaml:"poll_interval"`
	Retention     *int           `yaml:"retention"`
	Verify        *string        `yaml:"verify"`
	Approval      *string        `yaml:"approval"`
	Prereleases   *bool          `yaml:"prereleases"`
	MinReleaseAge *time.Duration `yaml:"min_release_age"`
}

// AssetRule maps release assets matching Match to a deb suite and/or an rpm
// release. Rules are evaluated in order; the first match wins.
type AssetRule struct {
	Match   string `yaml:"match"`
	Suite   string `yaml:"suite"`
	Release string `yaml:"release"`
}

// Settings are a source's effective per-source values.
type Settings struct {
	PollInterval  time.Duration
	Retention     int
	Verify        string
	Approval      string
	Prereleases   bool
	Recursive     bool
	MinReleaseAge time.Duration
}

// Settings resolves s's overrides against the configuration's defaults.
func (c *Config) Settings(s Source) Settings {
	d := c.Defaults
	out := Settings{
		PollInterval:  d.PollInterval,
		Retention:     d.Retention,
		Verify:        d.Verify,
		Approval:      d.Approval,
		Prereleases:   d.Prereleases,
		Recursive:     true,
		MinReleaseAge: d.MinReleaseAge,
	}
	if s.PollInterval != nil {
		out.PollInterval = *s.PollInterval
	}
	if s.Retention != nil {
		out.Retention = *s.Retention
	}
	if s.Verify != nil {
		out.Verify = *s.Verify
	}
	if s.Approval != nil {
		out.Approval = *s.Approval
	}
	if s.Prereleases != nil {
		out.Prereleases = *s.Prereleases
	}
	if s.Recursive != nil {
		out.Recursive = *s.Recursive
	}
	if s.MinReleaseAge != nil {
		out.MinReleaseAge = *s.MinReleaseAge
	}
	return out
}

// Default returns a configuration with every default from the design filled
// in. Loading decodes the file on top of it, so keys the file leaves out keep
// these values. Both format sections are filled in, so a section the file
// sets keeps the defaults of the keys it leaves out; Parse then drops the
// sections the file does not have.
func Default() *Config {
	return &Config{
		Server: Server{
			Listen:     ":8080",
			ClientName: "garner",
		},
		Log:      Log{Format: "json", Level: "info"},
		Database: Database{URL: "sqlite:///var/lib/garner/garner.db"},
		Storage: Storage{
			Path:              "/var/lib/garner",
			GCGrace:           10 * time.Minute,
			IndexHistory:      time.Hour,
			SnapshotRetention: 30,
			MaxPackageSize:    2 << 30,
		},
		Deb: defaultDeb(),
		RPM: defaultRPM(),
		OCI: defaultOCI(),
		Defaults: Defaults{
			Retention:    3,
			PollInterval: time.Hour,
			Verify:       "opportunistic",
			Approval:     "auto",
		},
	}
}

func defaultDeb() *Deb {
	return &Deb{Architectures: []string{"amd64"}, Suites: []string{"stable"}}
}

func defaultRPM() *RPM {
	return &RPM{Architectures: []string{"x86_64"}, Releases: []string{"stable"}}
}

func defaultOCI() *OCI {
	return &OCI{Retention: 10, UploadExpiry: 24 * time.Hour}
}

// IsRegistry reports whether the source syncs images from a registry
// rather than releases from a forge.
func (s Source) IsRegistry() bool { return s.Registry != "" }

// LocalName is the name a registry source's images are stored under.
func (s Source) LocalName() string {
	if s.As != "" {
		return s.As
	}
	return s.Image
}

// Formats returns the names of the configured format sections, sorted.
func (c *Config) Formats() []string {
	var out []string
	if c.Deb != nil {
		out = append(out, "deb")
	}
	if c.OCI != nil {
		out = append(out, "oci")
	}
	if c.RPM != nil {
		out = append(out, "rpm")
	}
	return out
}

const defaultMaxConcurrent = 4

// Load reads, decodes and validates the file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates a configuration file's contents.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, decodeError(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &ValidationError{Problems: []string{"the file contains more than one YAML document"}}
	}
	// Decoding leaves a preset section alone when the file lacks it, and
	// sets it to nil for a bare "deb:". Presence is what enables a format.
	keys := topLevelKeys(data)
	if !keys["deb"] {
		cfg.Deb = nil
	} else if cfg.Deb == nil {
		cfg.Deb = defaultDeb()
	}
	if !keys["rpm"] {
		cfg.RPM = nil
	} else if cfg.RPM == nil {
		cfg.RPM = defaultRPM()
	}
	if !keys["oci"] {
		cfg.OCI = nil
	} else if cfg.OCI == nil {
		cfg.OCI = defaultOCI()
	}
	for i := range cfg.Forges {
		if cfg.Forges[i].MaxConcurrent == 0 {
			cfg.Forges[i].MaxConcurrent = defaultMaxConcurrent
		}
	}
	for i := range cfg.Registries {
		if cfg.Registries[i].MaxConcurrent == 0 {
			cfg.Registries[i].MaxConcurrent = defaultMaxConcurrent
		}
	}
	sum := sha256.Sum256(data)
	cfg.Hash = hex.EncodeToString(sum[:])
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// topLevelKeys returns the keys of the document's top-level mapping, with
// merge keys resolved. The document has already decoded, so errors are not
// possible here.
func topLevelKeys(data []byte) map[string]bool {
	var m map[string]any
	keys := map[string]bool{}
	if yaml.Unmarshal(data, &m) != nil {
		return keys
	}
	for k := range m {
		keys[k] = true
	}
	return keys
}

// decodeError turns a YAML decoding error into a ValidationError, keeping
// the decoder's "line N: ..." messages.
func decodeError(err error) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return &ValidationError{Problems: te.Errors}
	}
	return &ValidationError{Problems: []string{err.Error()}}
}

// ValidationError lists every problem found in a configuration file.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "invalid configuration (%d problems):", len(e.Problems))
	if len(e.Problems) == 1 {
		b.Reset()
		b.WriteString("invalid configuration:")
	}
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		b.WriteString(p)
	}
	return b.String()
}
