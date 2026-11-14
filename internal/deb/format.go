package deb

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/fsutil"
)

// Format is the deb implementation of format.Format.
type Format struct{}

var _ format.Format = Format{}

func (Format) Name() string            { return "deb" }
func (Format) Namespace() string       { return "os" }
func (Format) ArchIndependent() string { return "all" }

func (Format) Detect(head []byte) bool {
	return bytes.HasPrefix(head, []byte("!<arch>\ndebian-binary"))
}

// metadata is what garner keeps from a .deb besides its identity.
type metadata struct {
	Control [][2]string `json:"control"`
	MD5     string      `json:"md5"`
}

func (Format) Parse(f *os.File) (*format.Parsed, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	h := md5.New()
	p, err := Read(io.TeeReader(f, h))
	if err != nil {
		return nil, err
	}
	m := metadata{MD5: hex.EncodeToString(h.Sum(nil))}
	for _, fl := range p.Control {
		m.Control = append(m.Control, [2]string{fl.Name, fl.Value})
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	version := p.Version.Upstream
	if p.Version.Revision != "" {
		version += "-" + p.Version.Revision
	}
	return &format.Parsed{
		Name:     p.Name,
		Epoch:    int(p.Version.Epoch),
		Version:  version,
		Arch:     p.Arch,
		Metadata: raw,
	}, nil
}

// version rebuilds a package's dpkg version from its stored columns.
func version(p *format.Package) Version {
	v, err := ParseVersion(p.Version)
	if err != nil {
		// Stored versions were parsed at ingest; fall back to raw order.
		return Version{Upstream: p.Version}
	}
	v.Epoch = uint32(p.Epoch)
	return v
}

func (Format) Compare(a, b *format.Package) int { return Compare(version(a), version(b)) }

func (Format) Distributions(c *config.Config) []string { return c.Deb.Suites }
func (Format) Architectures(c *config.Config) []string { return c.Deb.Architectures }

// PoolPath is main/<prefix>/<name>/<hash16>/<name>_<version>_<arch>.deb
// (section 4); the version has no epoch, as in Debian's own file names.
func (Format) PoolPath(p *format.Package) string {
	prefix := p.Name[:1]
	if strings.HasPrefix(p.Name, "lib") && len(p.Name) > 3 {
		prefix = p.Name[:4]
	}
	file := fmt.Sprintf("%s_%s_%s.deb", p.Name, p.Version, p.Arch)
	return path.Join("main", prefix, p.Name, p.ServedSHA256()[:16], file)
}

func (Format) Prepare(context.Context, string, string) (string, error) { return "", nil }

func (Format) Links(c *config.Config) []format.Link {
	return []format.Link{
		{Path: "deb/dists", Target: "../current/deb/dists"},
		{Path: "deb/pool", Target: "../../pool/deb"},
	}
}

func (Format) Client(c *config.Config, publicURL string) format.ClientFile {
	name := c.Server.ClientName
	suite := c.Deb.Suites[0]
	if slices.Contains(c.Deb.Suites, "stable") {
		suite = "stable"
	}
	content := fmt.Sprintf("Types: deb\nURIs: %s/deb\nSuites: %s\nComponents: main\nSigned-By: /usr/share/keyrings/%s.gpg\n",
		publicURL, suite, name)
	instr := fmt.Sprintf(`sudo curl -fsSLo /usr/share/keyrings/%[2]s.gpg %[1]s/key.gpg
sudo curl -fsSLo /etc/apt/sources.list.d/%[2]s.sources %[1]s/%[2]s.sources
sudo apt update`, publicURL, name)
	if len(c.Deb.Suites) > 1 {
		instr += fmt.Sprintf("\n# Suites: %s. Edit Suites: in the .sources file to pick another.",
			strings.Join(c.Deb.Suites, ", "))
	}
	return format.ClientFile{Name: name + ".sources", Content: []byte(content), Instructions: instr}
}

// indexFile is one file listed in Release.
type indexFile struct {
	path   string // relative to dists/<suite>
	data   []byte
	sha256 string
}

func (Format) Render(ctx context.Context, r *format.RenderRequest) error {
	c := r.Config
	now := time.Now().UTC()
	for _, suite := range c.Deb.Suites {
		suiteDir := filepath.Join(r.Dir, "dists", suite)
		var files []indexFile
		for _, arch := range c.Deb.Architectures {
			var pkgs []*format.Package
			for _, p := range r.Packages {
				if p.Distribution == suite && (p.Arch == arch || p.Arch == "all") {
					pkgs = append(pkgs, p)
				}
			}
			slices.SortFunc(pkgs, func(a, b *format.Package) int {
				if c := strings.Compare(a.Name, b.Name); c != 0 {
					return c
				}
				if c := Compare(version(a), version(b)); c != 0 {
					return c
				}
				return strings.Compare(a.Arch, b.Arch)
			})
			packages, err := packagesFile(pkgs)
			if err != nil {
				return err
			}
			dir := path.Join("main", "binary-"+arch)
			gz, err := gzipBytes(packages)
			if err != nil {
				return err
			}
			xzd, err := xzBytes(packages)
			if err != nil {
				return err
			}
			for _, f := range []indexFile{
				{path: dir + "/Packages", data: packages},
				{path: dir + "/Packages.gz", data: gz},
				{path: dir + "/Packages.xz", data: xzd},
			} {
				sum := sha256.Sum256(f.data)
				f.sha256 = hex.EncodeToString(sum[:])
				files = append(files, f)
			}
		}
		for _, f := range files {
			full := filepath.Join(suiteDir, filepath.FromSlash(f.path))
			if err := fsutil.WriteFile(full, f.data, 0o644); err != nil {
				return err
			}
			byHash := filepath.Join(filepath.Dir(full), "by-hash", "SHA256", f.sha256)
			if err := fsutil.LinkOrCopy(full, byHash); err != nil {
				return err
			}
		}
		if err := carryHistory(r.History, suite, c.Deb.Architectures, suiteDir); err != nil {
			return err
		}
		release := releaseFile(c, suite, now, files)
		if err := fsutil.WriteFile(filepath.Join(suiteDir, "Release"), release, 0o644); err != nil {
			return err
		}
		var inRelease, detached bytes.Buffer
		if err := r.Signer.ClearSign(&inRelease, release); err != nil {
			return fmt.Errorf("sign InRelease: %w", err)
		}
		if err := r.Signer.DetachSign(&detached, release); err != nil {
			return fmt.Errorf("sign Release.gpg: %w", err)
		}
		if err := fsutil.WriteFile(filepath.Join(suiteDir, "InRelease"), inRelease.Bytes(), 0o644); err != nil {
			return err
		}
		if err := fsutil.WriteFile(filepath.Join(suiteDir, "Release.gpg"), detached.Bytes(), 0o644); err != nil {
			return err
		}
	}
	// Lets clients pinned to /snapshots/<id>/deb resolve pool/ paths.
	return os.Symlink("../../../pool/deb", filepath.Join(r.Dir, "pool"))
}

// carryHistory hardlinks the by-hash files of earlier snapshots into this
// one, so a client holding an older InRelease can still fetch the indexes
// it lists (section 9, step 4).
func carryHistory(history []string, suite string, archs []string, suiteDir string) error {
	for _, old := range history {
		for _, arch := range archs {
			rel := filepath.Join("main", "binary-"+arch, "by-hash", "SHA256")
			entries, err := os.ReadDir(filepath.Join(old, "dists", suite, rel))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			for _, e := range entries {
				src := filepath.Join(old, "dists", suite, rel, e.Name())
				if err := fsutil.LinkOrCopy(src, filepath.Join(suiteDir, rel, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// packageFields are computed by garner; values from the control file are
// dropped so they cannot contradict the served file.
var packageFields = []string{"Filename", "Size", "MD5sum", "SHA1", "SHA256", "SHA512"}

func packagesFile(pkgs []*format.Package) ([]byte, error) {
	var b bytes.Buffer
	for i, p := range pkgs {
		var m metadata
		if err := json.Unmarshal(p.Metadata, &m); err != nil {
			return nil, fmt.Errorf("package %s: %w", p.ID, err)
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "Package: %s\n", p.Name)
		for _, f := range m.Control {
			if strings.EqualFold(f[0], "Package") || slices.ContainsFunc(packageFields, func(s string) bool {
				return strings.EqualFold(s, f[0])
			}) {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n", f[0], f[1])
		}
		fmt.Fprintf(&b, "Filename: pool/%s\n", Format{}.PoolPath(p))
		fmt.Fprintf(&b, "Size: %d\n", p.ServedSize)
		fmt.Fprintf(&b, "MD5sum: %s\n", m.MD5)
		fmt.Fprintf(&b, "SHA256: %s\n", p.ServedSHA256())
	}
	return b.Bytes(), nil
}

func releaseFile(c *config.Config, suite string, now time.Time, files []indexFile) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Origin: %s\nLabel: %s\nSuite: %s\nCodename: %s\n", c.Server.ClientName, c.Server.ClientName, suite, suite)
	fmt.Fprintf(&b, "Date: %s\n", now.Format("Mon, 02 Jan 2006 15:04:05 UTC"))
	fmt.Fprintf(&b, "Architectures: %s\nComponents: main\nAcquire-By-Hash: yes\n", strings.Join(c.Deb.Architectures, " "))
	b.WriteString("MD5Sum:\n")
	for _, f := range files {
		sum := md5.Sum(f.data)
		fmt.Fprintf(&b, " %s %d %s\n", hex.EncodeToString(sum[:]), len(f.data), f.path)
	}
	b.WriteString("SHA256:\n")
	for _, f := range files {
		fmt.Fprintf(&b, " %s %d %s\n", f.sha256, len(f.data), f.path)
	}
	return b.Bytes()
}

// gzipBytes compresses without a timestamp, so equal input gives equal
// output and therefore equal by-hash names.
func gzipBytes(data []byte) ([]byte, error) {
	var b bytes.Buffer
	w, err := gzip.NewWriterLevel(&b, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func xzBytes(data []byte) ([]byte, error) {
	var b bytes.Buffer
	w, err := xz.NewWriter(&b)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
