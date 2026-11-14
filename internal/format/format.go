// Package format defines what a package format provides to garner's core
// (design section 17). The core never branches on a format's name: deb and
// rpm, and later formats, are implementations registered in a Registry.
package format

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/signing"
)

// Parsed is what a format reads from a package file.
type Parsed struct {
	Name    string
	Epoch   int
	Version string
	Release string // "" for formats without one
	Arch    string // "" for formats without one
	// Metadata is format-specific and stored as JSON.
	Metadata json.RawMessage
}

// Package is a stored package as the core sees it, in one distribution.
type Package struct {
	ID           string
	Format       string
	Name         string
	Epoch        int
	Version      string
	Release      string
	Arch         string
	BlobSHA256   string
	SignedSHA256 string // "" when the original is served
	Metadata     json.RawMessage
	Origin       string
	OriginRef    string
	VerifyStatus string
	Pinned       bool
	CreatedAt    string

	// Placement, set when the package is read with a distribution.
	Distribution string
	State        string

	// ServedSize is the size of the served blob.
	ServedSize int64
}

// ServedSHA256 is the blob clients download: the re-signed variant when
// there is one, otherwise the original.
func (p *Package) ServedSHA256() string {
	if p.SignedSHA256 != "" {
		return p.SignedSHA256
	}
	return p.BlobSHA256
}

// Link is a symlink in public/, relative to public/, with a fixed target.
type Link struct {
	Path   string
	Target string
}

// RenderRequest is everything a format needs to write its part of a
// snapshot.
type RenderRequest struct {
	// Dir is snapshots/<id>/<format>, created by the format.
	Dir string
	// Packages are the active, retained packages, one per placement.
	Packages []*Package
	Config   *config.Config
	Signer   signing.Signer
	// PoolDir is storage/pool/<format>; every package is already linked
	// at PoolDir/PoolPath.
	PoolDir string
	// History are the <format> directories of earlier snapshots whose
	// index files must stay reachable (section 9, step 4), newest first.
	History []string
}

// ClientFile is a generated client configuration file served at the root,
// such as garner.sources.
type ClientFile struct {
	Name    string
	Content []byte
	// Instructions are shell commands for the landing page.
	Instructions string
}

// Format is one package format.
type Format interface {
	Name() string
	// Namespace is the ownership namespace (section 17); deb and rpm share
	// "os".
	Namespace() string
	// Detect reports whether a file starting with head is in this format.
	Detect(head []byte) bool
	Parse(f *os.File) (*Parsed, error)
	// Compare orders two versions of one package name and architecture.
	Compare(a, b *Package) int
	// Distributions and Architectures are the configured ones.
	Distributions(c *config.Config) []string
	Architectures(c *config.Config) []string
	// ArchIndependent is the architecture listed under every configured
	// architecture ("all", "noarch").
	ArchIndependent() string
	// PoolPath is the package's path below PoolDir.
	PoolPath(p *Package) string
	// Prepare returns the path of a file to serve instead of the original,
	// such as a re-signed rpm, or "" to serve the original. tmpDir is
	// removed by the caller.
	Prepare(ctx context.Context, original, tmpDir string) (string, error)
	Render(ctx context.Context, r *RenderRequest) error
	// Links are the fixed symlinks in public/ for this format.
	Links(c *config.Config) []Link
	Client(c *config.Config, publicURL string) ClientFile
}

// Registry holds the enabled formats in a fixed order.
type Registry struct {
	formats []Format
}

// NewRegistry registers formats; names must be unique.
func NewRegistry(fs ...Format) (*Registry, error) {
	seen := map[string]bool{}
	for _, f := range fs {
		if seen[f.Name()] {
			return nil, fmt.Errorf("format %q registered twice", f.Name())
		}
		seen[f.Name()] = true
	}
	return &Registry{formats: fs}, nil
}

// All returns the formats in registration order.
func (r *Registry) All() []Format { return r.formats }

// Get returns the format with the given name.
func (r *Registry) Get(name string) (Format, bool) {
	for _, f := range r.formats {
		if f.Name() == name {
			return f, true
		}
	}
	return nil, false
}

// Detect returns the format of the file, reading its first bytes.
func (r *Registry) Detect(f *os.File) (Format, error) {
	head := make([]byte, 512)
	n, err := f.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	head = head[:n]
	for _, fm := range r.formats {
		if fm.Detect(head) {
			return fm, nil
		}
	}
	return nil, fmt.Errorf("unrecognized package format")
}
