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
func (Format) DistributionKey() string { return "suite" }
func (Format) Extensions() []string    { return []string{".deb"} }

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
	// The name is also a pool directory, but shorter than the file name.
	if err := format.CheckPoolFileName(poolFile(p.Name, version, p.Arch)); err != nil {
		return nil, err
	}
	return &format.Parsed{
		Name:     p.Name,
		Epoch:    int(p.Version.Epoch),
		Version:  version,
		Arch:     p.Arch,
		Replaces: p.Replaces,
		Metadata: raw,
	}, nil
}

// version rebuilds a package's dpkg version from its stored columns.
// The epoch goes back in front before parsing: without it, a colon in the
// upstream part would be read as the epoch separator.
func version(p *format.Package) Version {
	v, err := ParseVersion(fmt.Sprintf("%d:%s", p.Epoch, p.Version))
	if err != nil {
		// Stored versions were parsed at ingest; fall back to raw order.
		return Version{Upstream: p.Version}
	}
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
	return path.Join("main", prefix, p.Name, p.ServedSHA256()[:16], poolFile(p.Name, p.Version, p.Arch))
}

// poolFile is a package's file name in the pool, built from its control
// fields with the version's epoch left out.
func poolFile(name, version, arch string) string {
	return fmt.Sprintf("%s_%s_%s.deb", name, version, arch)
}

func (Format) Prepare(context.Context, string, string) (string, error) { return "", nil }

func (Format) Links(c *config.Config) []format.Link {
	return []format.Link{
		{Path: "deb/dists", Target: "../current/deb/dists"},
		{Path: "deb/pool", Target: "../../pool/deb"},
	}
}

// Clients returns a deb822 .sources file per suite with the keys inline in
// Signed-By. apt reads the file as root and hands the keys to its verifier
// itself, so no keyring file is needed that _apt could fail to read, as one
// created under umask 027 or 077 would be. apt 2.4 (Ubuntu 22.04), the
// oldest supported, accepts inline keys.
func (Format) Clients(c *config.Config, publicURL string, keys []byte) []format.ClientFile {
	name := c.Server.ClientName
	signedBy := inlineField(keys)
	var out []format.ClientFile
	for i, suite := range format.ClientOrder(c.Deb.Suites) {
		p := format.ClientPath(i, suite, name+".sources")
		content := fmt.Sprintf("Types: deb\nURIs: %s/deb\nSuites: %s\nComponents: main\nSigned-By:%s",
			publicURL, suite, signedBy)
		instr := fmt.Sprintf(`sudo curl -fsSLo /etc/apt/sources.list.d/%[2]s.sources %[1]s/%[3]s
sudo apt update`, publicURL, name, p)
		out = append(out, format.ClientFile{
			Name: name + ".sources", Path: p, Distribution: suite, Content: []byte(content), Instructions: instr,
		})
	}
	return out
}

// inlineField formats text as the continuation lines of a deb822 field:
// each line indented by a space, and an empty line written as " .".
func inlineField(text []byte) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(string(text), "\n"), "\n") {
		if line == "" {
			line = "."
		}
		b.WriteString("\n ")
		b.WriteString(line)
	}
	b.WriteString("\n")
	return b.String()
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
		if err := carryHistory(r.History, suite, suiteDir); err != nil {
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

// carryHistory hardlinks into this snapshot the index files that earlier
// snapshots listed in their own Release, so a client holding an older
// InRelease can still fetch them by hash (section 9, step 4). Files those
// snapshots carried themselves are not carried again, so history ends
// when a snapshot leaves the window.
func carryHistory(history []string, suite string, suiteDir string) error {
	for _, old := range history {
		oldSuite := filepath.Join(old, "dists", suite)
		release, err := os.ReadFile(filepath.Join(oldSuite, "Release"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, f := range releaseSHA256(release) {
			rel := filepath.Join(filepath.Dir(filepath.FromSlash(f.path)), "by-hash", "SHA256", f.sha256)
			src := filepath.Join(oldSuite, rel)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			if err := fsutil.LinkOrCopy(src, filepath.Join(suiteDir, rel)); err != nil {
				return err
			}
		}
	}
	return nil
}

// releaseSHA256 returns the entries of a Release file's SHA256 field.
func releaseSHA256(release []byte) []indexFile {
	p, err := ParseParagraph(release)
	if err != nil {
		return nil
	}
	v, _ := p.Get("SHA256")
	var out []indexFile
	for _, line := range strings.Split(v, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && !strings.Contains(f[2], "..") {
			out = append(out, indexFile{path: f[2], sha256: f[0]})
		}
	}
	return out
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

// Presentation puts deb under Linux packages in the UI.
func (Format) Presentation() format.Presentation {
	return format.Presentation{Ecosystem: "Linux packages", Label: "Debian / Ubuntu", Tool: "apt", Signed: true}
}
