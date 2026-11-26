package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
)

// Distribution is the one implicit distribution of images.
const Distribution = "default"

// Format is the oci format.
type Format struct{}

var (
	_ format.Format             = Format{}
	_ format.BlobPooler         = Format{}
	_ format.RetentionDefaulter = Format{}
	_ format.RegistryFormat     = Format{}
	_ format.Lister             = Format{}
	_ format.Presenter          = Format{}
)

func (Format) Name() string      { return "oci" }
func (Format) Namespace() string { return "oci" }

// Extensions is empty: images are not release assets.
func (Format) Extensions() []string { return nil }

// Detect matches nothing: images are pushed, not uploaded as files.
func (Format) Detect([]byte) bool { return false }

func (Format) Parse(*os.File) (*format.Parsed, error) {
	return nil, errors.New("images are pushed with docker push or synced from a registry, not uploaded")
}

// Compare orders an image's manifests by push time: tags are unordered.
func (Format) Compare(a, b *format.Package) int {
	if c := strings.Compare(a.CreatedAt, b.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

// DistributionKey is empty: no request names a distribution.
func (Format) DistributionKey() string { return "" }

func (Format) Distributions(*config.Config) []string { return []string{Distribution} }
func (Format) Architectures(*config.Config) []string { return nil }
func (Format) ArchIndependent() string               { return "" }

// BlobPoolPath is where a blob is linked below the pool: named by its
// digest, as /v2/<name>/blobs/<digest> asks for it.
func (Format) BlobPoolPath(sha string) string { return "sha256:" + sha }

func (f Format) PoolPath(p *format.Package) string { return f.BlobPoolPath(p.ServedSHA256()) }

func (Format) Prepare(context.Context, string, string) (string, error) { return "", nil }

func (Format) DefaultRetention(c *config.Config) int {
	if c.OCI == nil {
		return 0
	}
	return c.OCI.Retention
}

func (Format) FromRegistries() {}

// Links serves the snapshot's registry tree at /v2/.
func (Format) Links(*config.Config) []format.Link {
	return []format.Link{{Path: "v2", Target: "current/oci/v2"}}
}

func (Format) Clients(*config.Config, string, []byte) []format.ClientFile { return nil }

// meta decodes a package's metadata.
func meta(p *format.Package) (Metadata, error) {
	var m Metadata
	if err := json.Unmarshal(p.Metadata, &m); err != nil {
		return m, fmt.Errorf("%s@sha256:%s: metadata: %w", p.Name, p.BlobSHA256, err)
	}
	return m, nil
}

// manifestFile is the file of a manifest reference (tag or digest).
func manifestFile(dir, name, ref, mediaType string) string {
	suffix, _ := Suffix(mediaType)
	return filepath.Join(dir, "v2", filepath.FromSlash(name), "manifests", ref+"."+suffix)
}

// TagList is the body of /v2/<name>/tags/list.
type TagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// Render writes snapshots/<id>/oci/v2: per name, its manifests by digest
// and by tag, its tag list, and a link to the pool for its blobs.
func (f Format) Render(_ context.Context, r *format.RenderRequest) error {
	v2 := filepath.Join(r.Dir, "v2")
	if err := os.MkdirAll(v2, 0o755); err != nil {
		return err
	}
	byID := map[string]*format.Package{}
	names := map[string]bool{}
	for _, p := range r.Packages {
		byID[p.ID] = p
		names[p.Name] = true
	}
	read := func(sha string) ([]byte, error) {
		return os.ReadFile(filepath.Join(r.PoolDir, f.BlobPoolPath(sha)))
	}
	write := func(file string, content []byte) error {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		return os.WriteFile(file, content, 0o644)
	}
	types := map[string]string{} // package ID -> media type
	for _, p := range r.Packages {
		m, err := meta(p)
		if err != nil {
			return err
		}
		types[p.ID] = m.MediaType
		content, err := read(p.ServedSHA256())
		if err != nil {
			return err
		}
		if err := write(manifestFile(r.Dir, p.Name, "sha256:"+p.ServedSHA256(), m.MediaType), content); err != nil {
			return err
		}
		for _, c := range m.Manifests {
			h, err := ParseDigest(c.Digest)
			if err != nil {
				return err
			}
			body, err := read(h)
			if err != nil {
				return err
			}
			if err := write(manifestFile(r.Dir, p.Name, c.Digest, c.MediaType), body); err != nil {
				return err
			}
		}
	}
	tags := map[string][]string{}
	for _, t := range r.Tags {
		p, ok := byID[t.PackageID]
		if !ok {
			continue
		}
		content, err := read(p.ServedSHA256())
		if err != nil {
			return err
		}
		if err := write(manifestFile(r.Dir, t.Name, t.Tag, types[p.ID]), content); err != nil {
			return err
		}
		tags[t.Name] = append(tags[t.Name], t.Tag)
	}
	for name := range names {
		dir := filepath.Join(v2, filepath.FromSlash(name))
		list := TagList{Name: name, Tags: tags[name]}
		if list.Tags == nil {
			list.Tags = []string{}
		}
		slices.Sort(list.Tags)
		body, err := json.Marshal(list)
		if err != nil {
			return err
		}
		if err := write(filepath.Join(dir, "tags", "list"), body); err != nil {
			return err
		}
		// Relative, so the snapshot keeps working after its rename from
		// <id>.tmp, which is at the same depth.
		target, err := filepath.Rel(dir, r.PoolDir)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, filepath.Join(dir, "blobs")); err != nil {
			return err
		}
	}
	return nil
}

// Verify reads the snapshot's registry tree as a client does: every tag
// and manifest by digest, and the tag lists.
func (f Format) Verify(_ context.Context, r *format.VerifyRequest) []format.Problem {
	var out []format.Problem
	errorf := func(check, file, msg string, args ...any) {
		out = append(out, format.Errorf(check, file, msg, args...))
	}
	v2 := filepath.Join(r.Dir, "v2")
	if fi, err := os.Stat(v2); err != nil || !fi.IsDir() {
		errorf("oci-tree", v2, "the registry tree is missing")
		return out
	}
	byID := map[string]*format.Package{}
	names := map[string]bool{}
	expected := map[string]bool{} // files the tree must have, and no others
	check := func(file, digest string) {
		expected[file] = true
		body, err := os.ReadFile(file)
		if err != nil {
			errorf("oci-manifest", file, "%v", err)
			return
		}
		if got := Digest(body); got != digest {
			errorf("oci-manifest", file, "has digest %s, want %s", got, digest)
		}
	}
	for _, p := range r.Packages {
		byID[p.ID] = p
		names[p.Name] = true
		m, err := meta(p)
		if err != nil {
			errorf("oci-metadata", "", "%v", err)
			continue
		}
		digest := "sha256:" + p.ServedSHA256()
		check(manifestFile(r.Dir, p.Name, digest, m.MediaType), digest)
		for _, c := range m.Manifests {
			check(manifestFile(r.Dir, p.Name, c.Digest, c.MediaType), c.Digest)
		}
		blobs := filepath.Join(v2, filepath.FromSlash(p.Name), "blobs")
		if _, err := os.Stat(filepath.Join(blobs, f.BlobPoolPath(p.ServedSHA256()))); err != nil {
			errorf("oci-blobs", blobs, "%s@%s: %v", p.Name, digest, err)
		}
	}
	tags := map[string][]string{}
	for _, t := range r.Tags {
		p, ok := byID[t.PackageID]
		if !ok {
			errorf("oci-tag", "", "%s:%s points at a package the snapshot does not serve", t.Name, t.Tag)
			continue
		}
		m, err := meta(p)
		if err != nil {
			continue
		}
		check(manifestFile(r.Dir, t.Name, t.Tag, m.MediaType), "sha256:"+p.ServedSHA256())
		tags[t.Name] = append(tags[t.Name], t.Tag)
	}
	for name := range names {
		file := filepath.Join(v2, filepath.FromSlash(name), "tags", "list")
		body, err := os.ReadFile(file)
		if err != nil {
			errorf("oci-tags", file, "%v", err)
			continue
		}
		var list TagList
		if err := json.Unmarshal(body, &list); err != nil {
			errorf("oci-tags", file, "%v", err)
			continue
		}
		want := tags[name]
		slices.Sort(want)
		if list.Name != name || !slices.Equal(list.Tags, want) && (len(want) > 0 || len(list.Tags) > 0) {
			errorf("oci-tags", file, "lists %s %v, want %s %v", list.Name, list.Tags, name, want)
		}
	}
	// A manifest file nothing accounts for is served all the same.
	filepath.WalkDir(v2, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if path.Base(filepath.Dir(p)) == "manifests" && !expected[p] {
			errorf("oci-manifest", p, "is served, but the snapshot has no such tag or manifest")
		}
		return nil
	})
	return out
}

// Presentation puts images in their own tab, unsigned: clients do not
// check them with the repository key.
func (Format) Presentation() format.Presentation {
	return format.Presentation{Ecosystem: "Container images", Label: "Container images", Tool: "docker"}
}

// maxGroups and maxTags bound the summary and a row, however many images
// and tags there are.
const (
	maxGroups = 12
	maxTags   = 5
)

// namespaceOf is the top-level namespace of an image name, "" for a name
// without one.
func namespaceOf(name string) string {
	if i := strings.Index(name, "/"); i > 0 {
		return name[:i] + "/"
	}
	return ""
}

// Summary counts the images, tags and platforms, and the images per
// top-level namespace.
func (Format) Summary(c *config.Config, publicURL string, pkgs []*format.Package, tags []format.Tag) format.Summary {
	publicURL = registryURL(c, publicURL)
	host := registryHost(publicURL)
	sum := format.Summary{Usage: "docker pull " + host + "/<name>:<tag>\n" +
		"# to push: docker login " + host + " -u <token name> (the token secret is the password)"}
	if strings.HasPrefix(publicURL, "http://") {
		sum.Usage += "\n# served over http: add " + host + " to the client's insecure registries"
	}
	names := map[string]bool{}
	platforms := map[string]bool{}
	for _, p := range pkgs {
		names[p.Name] = true
		if m, err := meta(p); err == nil {
			for _, pl := range m.Platforms {
				if !strings.HasPrefix(pl, "unknown/") {
					platforms[pl] = true
				}
			}
		}
	}
	groups := map[string]int{}
	for n := range names {
		groups[namespaceOf(n)]++
	}
	sum.Stats = []format.Stat{{Label: "images", Count: len(names)}, {Label: "tags", Count: len(tags)},
		{Label: "platforms", Count: len(platforms)}}
	for g, n := range groups {
		label := g
		if label == "" {
			label = "(no namespace)"
		}
		sum.Groups = append(sum.Groups, format.Stat{Label: label, Count: n})
	}
	slices.SortFunc(sum.Groups, func(a, b format.Stat) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Label, b.Label)
	})
	if len(sum.Groups) > maxGroups {
		sum.More = len(sum.Groups) - maxGroups
		sum.Groups = sum.Groups[:maxGroups]
	}
	return sum
}

// Rows lists every image: its newest tags by push time, platforms and a
// pull command.
func (Format) Rows(c *config.Config, publicURL string, pkgs []*format.Package, tags []format.Tag) []format.Row {
	publicURL = registryURL(c, publicURL)
	host := registryHost(publicURL)
	byName := map[string][]*format.Package{}
	byID := map[string]*format.Package{}
	for _, p := range pkgs {
		byName[p.Name] = append(byName[p.Name], p)
		byID[p.ID] = p
	}
	tagsOf := map[string][]format.Tag{}
	for _, t := range tags {
		tagsOf[t.Name] = append(tagsOf[t.Name], t)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	slices.Sort(names)
	var rows []format.Row
	for _, n := range names {
		ts := tagsOf[n]
		// Newest push first: the tag's image's push time, then the name.
		slices.SortFunc(ts, func(a, b format.Tag) int {
			pa, pb := byID[a.PackageID], byID[b.PackageID]
			if pa != nil && pb != nil && pa.CreatedAt != pb.CreatedAt {
				return strings.Compare(pb.CreatedAt, pa.CreatedAt)
			}
			return strings.Compare(a.Tag, b.Tag)
		})
		row := format.Row{Name: n, Group: namespaceOf(n)}
		for i, t := range ts {
			if i == maxTags {
				row.MoreTags = len(ts) - maxTags
				row.TagsURL = publicURL + Prefix + n + "/tags/list"
				break
			}
			row.Tags = append(row.Tags, t.Tag)
		}
		var platforms []string
		for _, p := range byName[n] {
			if m, err := meta(p); err == nil {
				for _, pl := range m.Platforms {
					if !slices.Contains(platforms, pl) && !strings.HasPrefix(pl, "unknown/") {
						platforms = append(platforms, pl)
					}
				}
			}
		}
		slices.Sort(platforms)
		if len(platforms) > 0 {
			row.Facts = append(row.Facts, format.Fact{Label: "platforms", Value: strings.Join(platforms, ", ")})
		}
		row.Facts = append(row.Facts, format.Fact{Label: "manifests", Value: fmt.Sprint(len(byName[n]))})
		ref := ""
		switch {
		case slices.ContainsFunc(ts, func(t format.Tag) bool { return t.Tag == "latest" }):
			ref = ":latest"
		case len(ts) > 0:
			ref = ":" + ts[0].Tag
		default:
			newest := slices.MaxFunc(byName[n], func(a, b *format.Package) int { return Format{}.Compare(a, b) })
			ref = "@" + newest.Version
		}
		row.Copy = "docker pull " + host + "/" + n + ref
		row.CopyLabel = "Copy " + row.Copy
		rows = append(rows, row)
	}
	return rows
}

// registryURL is the scheme and host of oci.public_url, or of publicURL
// (server.public_url) when that is not set. Clients find the registry at
// /v2/ on the host root, so a path is dropped.
func registryURL(c *config.Config, publicURL string) string {
	if c != nil && c.OCI != nil && c.OCI.PublicURL != "" {
		publicURL = c.OCI.PublicURL
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return strings.TrimRight(publicURL, "/")
	}
	return u.Scheme + "://" + u.Host
}

// registryHost is the host part of the registry URL, which clients put
// in front of image names.
func registryHost(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return publicURL
	}
	return u.Host
}
