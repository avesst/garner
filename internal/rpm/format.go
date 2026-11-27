package rpm

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/fsutil"
	"github.com/avesst/garner/internal/signing"
)

// Format is the rpm implementation of format.Format. It needs createrepo_c,
// rpmsign and gpg (section 9 and 10); NewFormat finds them.
type Format struct {
	Signer func() signing.Signer

	createrepo string
	rpmsign    string
	gpg        string
	gpgconf    string
}

var _ format.Format = (*Format)(nil)

// NewFormat finds the external tools. It fails with the list of missing
// ones, so the caller can refuse to start with a clear message.
func NewFormat(signer func() signing.Signer) (*Format, error) {
	f := &Format{Signer: signer}
	var missing []string
	for _, t := range []struct {
		name string
		dst  *string
	}{{"createrepo_c", &f.createrepo}, {"rpmsign", &f.rpmsign}, {"gpg", &f.gpg}, {"gpgconf", &f.gpgconf},
		// gpg starts the agent itself, but cannot use a secret key without it.
		{"gpg-agent", new(string)}} {
		p, err := exec.LookPath(t.name)
		if err != nil {
			missing = append(missing, t.name)
			continue
		}
		*t.dst = p
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing tools: %s", strings.Join(missing, ", "))
	}
	return f, nil
}

func (*Format) Name() string            { return "rpm" }
func (*Format) Namespace() string       { return "os" }
func (*Format) ArchIndependent() string { return "noarch" }
func (*Format) DistributionKey() string { return "release" }
func (*Format) Extensions() []string    { return []string{".rpm"} }

func (*Format) Detect(head []byte) bool { return bytes.HasPrefix(head, leadMagic) }

type metadata struct {
	Summary   string `json:"summary,omitempty"`
	SourceRPM string `json:"sourcerpm"`
}

func (*Format) Parse(f *os.File) (*format.Parsed, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	p, err := ReadHeader(f)
	if err != nil {
		return nil, err
	}
	if err := format.CheckPoolFileName(poolFile(p.Name, p.Version, p.Release, p.Arch)); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(metadata{Summary: p.Summary, SourceRPM: p.SourceRPM})
	if err != nil {
		return nil, err
	}
	return &format.Parsed{Name: p.Name, Epoch: p.Epoch, Version: p.Version, Release: p.Release,
		Arch: p.Arch, Replaces: p.Obsoletes, Metadata: raw}, nil
}

func (*Format) Compare(a, b *format.Package) int {
	return CompareEVR(a.Epoch, a.Version, a.Release, b.Epoch, b.Version, b.Release)
}

func (*Format) Distributions(c *config.Config) []string { return c.RPM.Releases }
func (*Format) Architectures(c *config.Config) []string { return c.RPM.Architectures }

// PoolPath is <prefix>/<hash16>/<name>-<version>-<release>.<arch>.rpm, with
// <prefix> as for deb (section 4).
func (*Format) PoolPath(p *format.Package) string {
	prefix := p.Name[:1]
	if strings.HasPrefix(p.Name, "lib") && len(p.Name) > 3 {
		prefix = p.Name[:4]
	}
	return path.Join(prefix, p.ServedSHA256()[:16], poolFile(p.Name, p.Version, p.Release, p.Arch))
}

// poolFile is a package's file name in the pool, as rpmbuild names it.
func poolFile(name, version, release, arch string) string {
	return fmt.Sprintf("%s-%s-%s.%s.rpm", name, version, release, arch)
}

// Prepare re-signs a copy of the package with the repository key using
// rpmsign --resign, which replaces any existing signature (section 10).
// --addsign does the same in upstream rpm 4.x, but adds a signature in
// rpm 6 and fails on a signed package in EL 10's rpm 4.19.
func (f *Format) Prepare(ctx context.Context, original, tmpDir string) (string, error) {
	exp, ok := f.Signer().(signing.KeyExporter)
	if !ok {
		return "", errors.New("the signing backend cannot export a key for rpmsign")
	}
	key, err := exp.ArmoredPrivateKey()
	if err != nil {
		return "", err
	}
	home := filepath.Join(tmpDir, "gnupg")
	if err := os.Mkdir(home, 0o700); err != nil {
		return "", err
	}
	defer exec.Command(f.gpgconf, "--homedir", home, "--kill", "all").Run()
	keyFile := filepath.Join(tmpDir, "key.asc")
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(ctx, f.gpg, "--homedir", home, "--batch", "--import", keyFile).CombinedOutput(); err != nil {
		return "", fmt.Errorf("gpg --import: %v: %s", err, bytes.TrimSpace(out))
	}
	os.Remove(keyFile)

	signed := filepath.Join(tmpDir, "package.rpm")
	if err := copyFile(original, signed); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, f.rpmsign, "--resign",
		// "!" makes gpg use exactly this (sub)key, the one garner records
		// as signing, instead of choosing a signing subkey itself.
		"--define", "_gpg_name "+f.Signer().SigningFingerprint()+"!",
		"--define", "_gpg_path "+home,
		"--define", "__gpg "+f.gpg,
		"--define", "_openpgp_sign gpg",
		signed)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("rpmsign: %v: %s", err, bytes.TrimSpace(out))
	}
	return signed, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (*Format) Links(c *config.Config) []format.Link {
	var out []format.Link
	for _, rel := range c.RPM.Releases {
		for _, arch := range repoArches(c) {
			dir := path.Join("rpm", rel, arch)
			out = append(out,
				format.Link{Path: dir + "/repodata", Target: "../../../current/rpm/" + rel + "/" + arch + "/repodata"},
				format.Link{Path: dir + "/pool", Target: "../../../../pool/rpm"},
			)
		}
	}
	return out
}

func (*Format) Clients(c *config.Config, publicURL string, _ []byte) []format.ClientFile {
	name := c.Server.ClientName
	var out []format.ClientFile
	for i, rel := range format.ClientOrder(c.RPM.Releases) {
		// One repo ID for every release, so installing another replaces it.
		p := format.ClientPath(i, rel, name+".repo")
		content := fmt.Sprintf(`[%[1]s]
name=%[1]s
baseurl=%[2]s/rpm/%[3]s/$basearch/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=%[2]s/key.asc
`, name, publicURL, rel)
		instr := fmt.Sprintf("sudo curl -fsSLo /etc/yum.repos.d/%[2]s.repo %[1]s/%[3]s", publicURL, name, p)
		out = append(out, format.ClientFile{
			Name: name + ".repo", Path: p, Distribution: rel, Content: []byte(content), Instructions: instr,
		})
	}
	return out
}

// Render runs createrepo_c for every release and architecture, signs
// repomd.xml and carries over earlier repodata (section 9).
func (f *Format) Render(ctx context.Context, r *format.RenderRequest) error {
	c := r.Config
	for _, rel := range c.RPM.Releases {
		for _, arch := range repoArches(c) {
			out := filepath.Join(r.Dir, rel, arch)
			if err := f.renderRepo(ctx, r, rel, arch, out); err != nil {
				return fmt.Errorf("%s/%s: %w", rel, arch, err)
			}
		}
	}
	return nil
}

func (f *Format) renderRepo(ctx context.Context, r *format.RenderRequest, rel, arch, out string) error {
	// createrepo_c records paths relative to the directory it scans, so
	// the pool is mirrored as hardlinks into a work directory.
	work := filepath.Join(r.Dir, ".work-"+rel+"-"+arch)
	defer os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	for _, p := range r.Packages {
		if p.Distribution != rel || !inRepo(r.Config, p.Arch, arch) {
			continue
		}
		rp := filepath.FromSlash(f.PoolPath(p))
		if err := fsutil.LinkOrCopy(filepath.Join(r.PoolDir, rp), filepath.Join(work, "pool", rp)); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, f.createrepo, "--quiet", "--no-database", "--unique-md-filenames",
		"--general-compress-type=zstd", "--outputdir", out, work)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("createrepo_c: %v: %s", err, bytes.TrimSpace(b))
	}
	repodata := filepath.Join(out, "repodata")
	repomd, err := os.ReadFile(filepath.Join(repodata, "repomd.xml"))
	if err != nil {
		return err
	}
	var sig bytes.Buffer
	if err := r.Signer.DetachSign(&sig, repomd); err != nil {
		return fmt.Errorf("sign repomd.xml: %w", err)
	}
	if err := fsutil.WriteFile(filepath.Join(repodata, "repomd.xml.asc"), sig.Bytes(), 0o644); err != nil {
		return err
	}
	for _, old := range r.History {
		if err := carryRepodata(filepath.Join(old, rel, arch, "repodata"), repodata); err != nil {
			return err
		}
	}
	// Lets clients pinned to /snapshots/<id>/rpm/<rel>/<arch> resolve pool/.
	return os.Symlink("../../../../../pool/rpm", filepath.Join(out, "pool"))
}

// carryRepodata hardlinks the metadata files an earlier repomd.xml listed
// into the new repodata directory. Their names carry their hash, so they
// never clash; files the earlier snapshot carried itself are not listed in
// its repomd.xml and are not carried again.
func carryRepodata(oldDir, newDir string) error {
	data, err := os.ReadFile(filepath.Join(oldDir, "repomd.xml"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var md struct {
		Data []struct {
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(data, &md); err != nil {
		return fmt.Errorf("%s: %w", oldDir, err)
	}
	for _, d := range md.Data {
		name := path.Base(d.Location.Href)
		if name == "." || name == "/" || strings.HasPrefix(name, "repomd") {
			continue
		}
		src := filepath.Join(oldDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := fsutil.LinkOrCopy(src, filepath.Join(newDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// Presentation puts rpm under Linux packages in the UI.
func (*Format) Presentation() format.Presentation {
	return format.Presentation{Ecosystem: "Linux packages", Label: "Fedora / RHEL", Tool: "dnf", Signed: true}
}
