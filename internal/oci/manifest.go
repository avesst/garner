// Package oci is the OCI image format (design section 19): manifests and
// image indexes pushed to garner or synced from registries, served for
// pulls from the snapshot under /v2/.
package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"regexp"
	"slices"
	"strings"
)

// Media types garner stores, with the file name suffix that carries each
// in the static tree: a static server picks Content-Type from the name.
const (
	MediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	MediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	MediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// kinds maps each media type to its suffix, in the order a server tries
// them.
var kinds = []struct{ mediaType, suffix string }{
	{MediaOCIIndex, "oci-index"},
	{MediaOCIManifest, "oci-manifest"},
	{MediaDockerList, "docker-list"},
	{MediaDockerManifest, "docker-manifest"},
}

// Suffix returns the file name suffix of a manifest media type.
func Suffix(mediaType string) (string, bool) {
	for _, k := range kinds {
		if k.mediaType == mediaType {
			return k.suffix, true
		}
	}
	return "", false
}

// MediaTypes lists the manifest media types garner accepts, for Accept
// headers.
func MediaTypes() []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = k.mediaType
	}
	return out
}

func isIndex(mediaType string) bool {
	return mediaType == MediaOCIIndex || mediaType == MediaDockerList
}

// MaxManifestSize bounds a manifest garner accepts or fetches.
const MaxManifestSize = 4 << 20

var (
	// Repository names of the distribution spec.
	reName = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	reTag  = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	reHex  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// reserved are path components a name may not have: in the static tree
// they are the directories of the name above it (section 19).
var reserved = []string{"blobs", "manifests", "tags", "uploads", "referrers"}

// ValidName returns why name is not an acceptable repository name, or
// nil.
func ValidName(name string) error {
	if len(name) > 255 {
		return fmt.Errorf("repository name is longer than 255 characters")
	}
	if !reName.MatchString(name) {
		return fmt.Errorf("invalid repository name %q", name)
	}
	for _, c := range strings.Split(name, "/") {
		if slices.Contains(reserved, c) {
			return fmt.Errorf("repository name %q has the reserved component %q", name, c)
		}
	}
	return nil
}

// ValidTag reports whether tag is a valid tag.
func ValidTag(tag string) bool { return reTag.MatchString(tag) }

// ParseDigest returns the hex sha256 of a "sha256:<hex>" digest.
func ParseDigest(d string) (string, error) {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok || !reHex.MatchString(hexPart) {
		return "", fmt.Errorf("unsupported digest %q: only sha256 is supported", d)
	}
	return hexPart, nil
}

// Digest is the digest of content.
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Descriptor points at content by digest.
type Descriptor struct {
	MediaType    string    `json:"mediaType"`
	Digest       string    `json:"digest"`
	Size         int64     `json:"size"`
	URLs         []string  `json:"urls,omitempty"`
	Platform     *Platform `json:"platform,omitempty"`
	ArtifactType string    `json:"artifactType,omitempty"`
}

// Platform is a child manifest's platform in an index.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

func (p *Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Manifest is an image manifest or index, as far as garner reads it.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        *Descriptor  `json:"config"`
	Layers        []Descriptor `json:"layers"`
	Manifests     []Descriptor `json:"manifests"`
	Subject       *Descriptor  `json:"subject"`
}

// ErrUnsupported marks a manifest type garner does not store, such as
// Docker schema 1.
var ErrUnsupported = errors.New("unsupported manifest media type")

// ParseManifest parses a manifest. contentType is the media type the
// sender declared; the body's own mediaType must agree with it.
func ParseManifest(body []byte, contentType string) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %v", err)
	}
	declared := ""
	if contentType != "" {
		if mt, _, err := mime.ParseMediaType(contentType); err == nil {
			declared = mt
		}
	}
	switch {
	case m.MediaType != "" && declared != "" && declared != m.MediaType && declared != "application/json" &&
		declared != "application/octet-stream":
		return nil, fmt.Errorf("Content-Type %s does not match the manifest's mediaType %s", declared, m.MediaType)
	case m.MediaType == "" && declared != "" && declared != "application/json" && declared != "application/octet-stream":
		m.MediaType = declared
	case m.MediaType == "" && m.Manifests != nil:
		m.MediaType = MediaOCIIndex
	case m.MediaType == "" && m.Config != nil:
		m.MediaType = MediaOCIManifest
	}
	if _, ok := Suffix(m.MediaType); !ok {
		return nil, fmt.Errorf("%w %q", ErrUnsupported, m.MediaType)
	}
	if m.SchemaVersion != 2 {
		return nil, fmt.Errorf("schemaVersion must be 2, got %d", m.SchemaVersion)
	}
	if isIndex(m.MediaType) {
		if m.Config != nil || len(m.Layers) > 0 {
			return nil, fmt.Errorf("an index has no config or layers")
		}
		for _, d := range m.Manifests {
			if _, err := ParseDigest(d.Digest); err != nil {
				return nil, err
			}
			if _, ok := Suffix(d.MediaType); !ok {
				return nil, fmt.Errorf("index entry %s: %w %q", d.Digest, ErrUnsupported, d.MediaType)
			}
		}
		return &m, nil
	}
	if m.Config == nil {
		return nil, fmt.Errorf("manifest has no config")
	}
	if len(m.Manifests) > 0 {
		return nil, fmt.Errorf("an image manifest has no manifests list")
	}
	for _, d := range append([]Descriptor{*m.Config}, m.Layers...) {
		if _, err := ParseDigest(d.Digest); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// Blobs returns the hex sha256s of the config and layers an image
// manifest needs stored. Foreign layers, which clients fetch from their
// URLs, are left out.
func (m *Manifest) Blobs() []string {
	var out []string
	for _, d := range m.stored() {
		h, _ := ParseDigest(d.Digest)
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// stored returns the descriptors of the blobs Blobs lists.
func (m *Manifest) stored() []Descriptor {
	if isIndex(m.MediaType) {
		return nil
	}
	var out []Descriptor
	for _, d := range append([]Descriptor{*m.Config}, m.Layers...) {
		if len(d.URLs) == 0 {
			out = append(out, d)
		}
	}
	return out
}

// Child is a manifest an index refers to, directly or through a nested
// index.
type Child struct {
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
}

// Metadata is what garner stores about an image package.
type Metadata struct {
	MediaType string   `json:"media_type"`
	Size      int64    `json:"size"`
	Manifests []Child  `json:"manifests,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
}

// Tree is a manifest with everything below it: what a package owns.
type Tree struct {
	Digest   string
	Content  []byte
	Manifest *Manifest
	// Blobs are the hex sha256s of every blob below the manifest: child
	// manifests, configs and layers, each once.
	Blobs []string
	// Sizes are the sizes the descriptors declare for Blobs, which sync
	// checks before downloading.
	Sizes map[string]int64
	Meta  Metadata
}

// Fetch returns a stored manifest's content by hex sha256, or an error
// that wraps ErrMissing when it is not stored.
type Fetch func(sha string) ([]byte, error)

// ErrMissing marks content a manifest refers to that is not stored.
var ErrMissing = errors.New("not stored")

// Walk builds the tree of a manifest whose children, if it is an index,
// are stored and read with fetch. Nested indexes are followed.
func Walk(content []byte, contentType string, fetch Fetch) (*Tree, error) {
	m, err := ParseManifest(content, contentType)
	if err != nil {
		return nil, err
	}
	t := &Tree{Digest: Digest(content), Content: content, Manifest: m, Sizes: map[string]int64{},
		Meta: Metadata{MediaType: m.MediaType, Size: int64(len(content))}}
	seen := map[string]bool{}
	add := func(d Descriptor) {
		h, _ := ParseDigest(d.Digest)
		if !seen[h] {
			seen[h] = true
			t.Blobs = append(t.Blobs, h)
			t.Sizes[h] = d.Size
		}
	}
	for _, d := range m.stored() {
		add(d)
	}
	var walk func(m *Manifest, depth int) error
	walk = func(m *Manifest, depth int) error {
		if depth > 4 {
			return fmt.Errorf("indexes nested more than 4 deep")
		}
		for _, d := range m.Manifests {
			h, _ := ParseDigest(d.Digest)
			if d.Platform != nil && depth == 0 {
				t.Meta.Platforms = append(t.Meta.Platforms, d.Platform.String())
			}
			if seen[h] {
				continue
			}
			body, err := fetch(h)
			if err != nil {
				return fmt.Errorf("manifest %s: %w", d.Digest, err)
			}
			if Digest(body) != d.Digest {
				return fmt.Errorf("manifest %s: stored content has another digest", d.Digest)
			}
			child, err := ParseManifest(body, d.MediaType)
			if err != nil {
				return fmt.Errorf("manifest %s: %w", d.Digest, err)
			}
			if child.MediaType != d.MediaType {
				return fmt.Errorf("manifest %s is %s, the index says %s", d.Digest, child.MediaType, d.MediaType)
			}
			add(d)
			t.Meta.Manifests = append(t.Meta.Manifests, Child{Digest: d.Digest, MediaType: child.MediaType})
			for _, b := range child.stored() {
				add(b)
			}
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(m, 0); err != nil {
		return nil, err
	}
	return t, nil
}
