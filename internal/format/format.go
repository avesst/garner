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
	"slices"
	"strings"

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
	// Replaces names the packages this one takes the place of on clients
	// (deb Replaces, rpm Obsoletes). Ownership applies to them as to Name
	// (section 8).
	Replaces []string
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

	// Blobs are the blobs the package owns besides BlobSHA256, such as an
	// image's layers (section 19); nil for formats with one file per
	// package. The publisher sets it.
	Blobs []string
}

// Tag is a mutable label on a package, such as an image tag (section 19).
type Tag struct {
	Format    string
	Name      string
	Tag       string
	PackageID string
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
	// Tags are the tags on Packages.
	Tags []Tag
}

// ClientFile is a generated client configuration file, such as
// garner.sources, for one distribution.
//
// Every distribution's file has the same Name and installs to the same
// local path, so a host uses one distribution of the repository at a time
// and picking another replaces it.
type ClientFile struct {
	Name string
	// Path is where the file is served, relative to the repository root.
	Path string
	// Distribution is the suite or release the file points at.
	Distribution string
	Content      []byte
	// Instructions are shell commands for the UI.
	Instructions string
}

// ClientsDir holds the client files of the non-default distributions.
const ClientsDir = "clients"

// ClientOrder returns the distributions to write client files for, the
// default first: "stable" if configured, else the first one.
func ClientOrder(ds []string) []string {
	out := slices.Clone(ds)
	if i := slices.Index(out, "stable"); i > 0 {
		out = append(append([]string{"stable"}, out[:i]...), out[i+1:]...)
	}
	return out
}

// DefaultDistribution is where a package goes when no distribution is
// named: the first of ClientOrder, so the same one whose client file is
// served at the root. It is "" when ds is empty.
func DefaultDistribution(ds []string) string {
	if o := ClientOrder(ds); len(o) > 0 {
		return o[0]
	}
	return ""
}

// ClientPath is where the client file name for the i-th distribution in
// ClientOrder is served: the default's at the root, so its URL does not
// change when distributions are added, the others in ClientsDir.
func ClientPath(i int, dist, name string) string {
	if i == 0 {
		return name
	}
	return ClientsDir + "/" + dist + "/" + name
}

// Format is one package format.
type Format interface {
	Name() string
	// Namespace is the ownership namespace (section 17); deb and rpm share
	// "os".
	Namespace() string
	// Extensions are the file name suffixes of this format's packages
	// (".deb"). Sync only downloads release assets with one of them.
	Extensions() []string
	// Detect reports whether a file starting with head is in this format.
	Detect(head []byte) bool
	Parse(f *os.File) (*Parsed, error)
	// Compare orders two versions of one package name and architecture.
	Compare(a, b *Package) int
	// DistributionKey names this format's distribution in requests and
	// asset rules ("suite", "release").
	DistributionKey() string
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
	// Clients returns one client file per distribution, in ClientOrder.
	Clients(c *config.Config, publicURL string) []ClientFile
	// Verify checks a rendered snapshot the way a client reads it, from
	// the signed top-level files down to the package files, against the
	// packages the snapshot serves (garner verify).
	Verify(ctx context.Context, r *VerifyRequest) []Problem
}

// BlobPooler is a format whose packages own several blobs (section 19).
// Each blob is linked once below the pool directory, at BlobPoolPath,
// whichever packages share it; PoolPath returns the path of BlobSHA256.
type BlobPooler interface {
	BlobPoolPath(sha string) string
}

// Presenter describes a format in the UI (section 14). The page
// has one tab per ecosystem: the formats that share a Namespace, such as
// deb and rpm. A format without it is shown under its own name.
type Presenter interface {
	Presentation() Presentation
}

// Presentation is how the UI shows a format.
type Presentation struct {
	// Ecosystem names the tab of the format's namespace ("Linux
	// packages"); formats of one namespace give the same one.
	Ecosystem string
	// Label and Tool name the format within its tab ("Debian / Ubuntu",
	// "apt").
	Label, Tool string
	// Signed reports whether clients check the format with the
	// repository key, so its tab shows the key.
	Signed bool
}

// Lister is a format that summarizes and lists its packages itself, in
// place of client files, the count grid and the generic package table
// (section 19).
type Lister interface {
	// Summary is the format's part of its ecosystem's tab. It must stay
	// small however many packages there are.
	Summary(c *config.Config, publicURL string, pkgs []*Package, tags []Tag) Summary
	// Rows list every package name for the list in the format's tab.
	Rows(c *config.Config, publicURL string, pkgs []*Package, tags []Tag) []Row
}

// Summary is a format's summary in its ecosystem's tab.
type Summary struct {
	// Usage is shell commands showing how clients use the format.
	Usage string
	// Stats are headline counts ("images", 12).
	Stats []Stat
	// Groups are counts per group of names, such as per top-level
	// namespace, largest first; More counts the groups left out.
	Groups []Stat
	More   int
}

// Stat is a labelled count.
type Stat struct {
	Label string
	Count int
}

// Row is one package name in an ecosystem's list.
type Row struct {
	Name string
	// Group orders the rows, such as by namespace; rows without one come
	// first.
	Group string
	// Facts describe the name: versions per distribution, platforms.
	Facts []Fact
	// Tags are the newest tags; MoreTags counts the rest, listed at
	// TagsURL.
	Tags     []string
	MoreTags int
	TagsURL  string
	// Copy is text a button copies, such as a pull command, with
	// CopyLabel naming what it copies.
	Copy, CopyLabel string
}

// Fact is a labelled detail of a Row.
type Fact struct {
	Label, Value string
}

// RegistryFormat is a format whose packages registry sources deliver,
// rather than forge sources (section 19).
type RegistryFormat interface {
	FromRegistries()
}

// RetentionDefaulter is a format with its own retention default (section
// 17), used for packages no source delivered.
type RetentionDefaulter interface {
	DefaultRetention(c *config.Config) int
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

// Names returns the registered formats' names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.formats))
	for _, f := range r.formats {
		out = append(out, f.Name())
	}
	slices.Sort(out)
	return out
}

// Get returns the format with the given name.
func (r *Registry) Get(name string) (Format, bool) {
	for _, f := range r.formats {
		if f.Name() == name {
			return f, true
		}
	}
	return nil, false
}

// HandlesName reports whether an enabled format uses the file name's
// extension, so a disabled format's files are never downloaded.
func (r *Registry) HandlesName(name string) bool {
	for _, f := range r.formats {
		for _, ext := range f.Extensions() {
			if strings.HasSuffix(name, ext) {
				return true
			}
		}
	}
	return false
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
