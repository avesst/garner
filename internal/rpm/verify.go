package rpm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/avesst/garner/internal/format"
)

// maxMetadataFile bounds what Verify reads of one repodata file, before
// and after decompression.
const maxMetadataFile = 1 << 30

// repomd is the part of repomd.xml Verify reads.
type repomd struct {
	Data []struct {
		Type     string `xml:"type,attr"`
		Checksum struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		OpenChecksum struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"open-checksum"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
		Size     int64 `xml:"size"`
		OpenSize int64 `xml:"open-size"`
	} `xml:"data"`
}

// primary is the part of primary.xml Verify reads.
type primary struct {
	Count    int `xml:"packages,attr"`
	Packages []struct {
		Name    string `xml:"name"`
		Arch    string `xml:"arch"`
		Version struct {
			Epoch string `xml:"epoch,attr"`
			Ver   string `xml:"ver,attr"`
			Rel   string `xml:"rel,attr"`
		} `xml:"version"`
		Checksum struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		Size struct {
			Package int64 `xml:"package,attr"`
		} `xml:"size"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"package"`
}

// Verify checks each configured release and architecture like dnf reads
// it: repomd.xml.asc against the published keys, every repodata file
// repomd.xml lists by checksum and size, compressed and not, the packages
// in primary against the pool and the snapshot's package set, and the
// repodata of the previous snapshot. It parses the files itself rather
// than trusting createrepo_c's output.
func (f *Format) Verify(ctx context.Context, r *format.VerifyRequest) []format.Problem {
	v := &rpmVerifier{r: r}
	for _, rel := range r.Config.RPM.Releases {
		for _, arch := range repoArches(r.Config) {
			if ctx.Err() != nil {
				v.errorf("cancelled", r.Dir, "%v", ctx.Err())
				return v.problems
			}
			v.repo(rel, arch)
		}
	}
	return v.problems
}

type rpmVerifier struct {
	r        *format.VerifyRequest
	problems []format.Problem
}

func (v *rpmVerifier) errorf(check, path, f string, args ...any) {
	v.problems = append(v.problems, format.Errorf(check, path, f, args...))
}

func (v *rpmVerifier) repo(rel, arch string) {
	repo := filepath.Join(v.r.Dir, rel, arch)
	repodata := filepath.Join(repo, "repodata")
	mdPath := filepath.Join(repodata, "repomd.xml")
	md, err := readLimited(mdPath)
	if err != nil {
		v.errorf("repomd", mdPath, "%v", err)
		return
	}
	ascPath := mdPath + ".asc"
	if sig, err := os.ReadFile(ascPath); err != nil {
		v.errorf("signature", ascPath, "%v", err)
	} else if err := v.r.Keys.VerifyDetached(md, sig); err != nil {
		v.errorf("signature", ascPath, "does not verify repomd.xml with the published keys: %v", err)
	}
	var m repomd
	if err := xml.Unmarshal(md, &m); err != nil {
		v.errorf("repomd", mdPath, "%v", err)
		return
	}

	var primaryXML []byte
	for _, d := range m.Data {
		href := filepath.FromSlash(d.Location.Href)
		if !filepath.IsLocal(href) {
			v.errorf("repomd", mdPath, "%s: location %q leaves the repository", d.Type, d.Location.Href)
			continue
		}
		full := filepath.Join(repo, href)
		data, err := readLimited(full)
		if err != nil {
			v.errorf("repodata", full, "listed in repomd.xml: %v", err)
			continue
		}
		if !checksumOK(d.Checksum.Type, d.Checksum.Value, data) || int64(len(data)) != d.Size {
			v.errorf("repodata", full, "size or %s checksum differs from repomd.xml", d.Checksum.Type)
			continue
		}
		if d.OpenChecksum.Value == "" {
			continue
		}
		plain, err := decompress(d.Location.Href, data)
		if err != nil {
			v.errorf("compression", full, "%v", err)
			continue
		}
		if !checksumOK(d.OpenChecksum.Type, d.OpenChecksum.Value, plain) || int64(len(plain)) != d.OpenSize {
			v.errorf("compression", full, "decompressed size or %s checksum differs from repomd.xml", d.OpenChecksum.Type)
			continue
		}
		if d.Type == "primary" {
			primaryXML = plain
		}
	}
	if primaryXML == nil {
		v.errorf("repomd", mdPath, "no usable primary metadata")
	} else {
		v.primary(rel, arch, repo, primaryXML)
	}
	if _, err := os.Stat(filepath.Join(repo, "pool")); err != nil {
		v.errorf("pool-link", filepath.Join(repo, "pool"), "snapshot pool link does not resolve: %v", err)
	}
	v.history(rel, arch, repodata)
}

// primary checks primary.xml against the packages the snapshot serves for
// rel and arch, and each package against its pool file.
func (v *rpmVerifier) primary(rel, arch, repo string, data []byte) {
	file := filepath.Join(repo, "repodata", "primary.xml")
	var p primary
	if err := xml.Unmarshal(data, &p); err != nil {
		v.errorf("primary", file, "%v", err)
		return
	}
	if p.Count != len(p.Packages) {
		v.errorf("primary", file, "says %d packages but lists %d", p.Count, len(p.Packages))
	}
	want := map[string]*format.Package{}
	for _, pk := range v.r.Packages {
		if pk.Distribution == rel && inRepo(v.r.Config, pk.Arch, arch) {
			want["pool/"+(&Format{}).PoolPath(pk)] = pk
		}
	}
	for _, e := range p.Packages {
		href := e.Location.Href
		pk, ok := want[href]
		if !ok {
			v.errorf("primary", file, "lists %s (%s), which the snapshot does not serve here", e.Name, href)
			continue
		}
		delete(want, href)
		epoch := strconv.Itoa(pk.Epoch)
		if e.Name != pk.Name || e.Arch != pk.Arch || e.Version.Ver != pk.Version || e.Version.Rel != pk.Release ||
			(e.Version.Epoch != epoch && !(e.Version.Epoch == "" && pk.Epoch == 0)) {
			v.errorf("primary", file, "%s: lists %s-%s:%s-%s.%s, want %s-%d:%s-%s.%s", href,
				e.Name, e.Version.Epoch, e.Version.Ver, e.Version.Rel, e.Arch, pk.Name, pk.Epoch, pk.Version, pk.Release, pk.Arch)
		}
		if e.Checksum.Type != "sha256" || e.Checksum.Value != pk.ServedSHA256() {
			v.errorf("primary", file, "%s: checksum is %s:%s, the snapshot serves %s", href, e.Checksum.Type, e.Checksum.Value, pk.ServedSHA256())
		}
		if e.Size.Package != pk.ServedSize {
			v.errorf("primary", file, "%s: size is %d, the served file has %d bytes", href, e.Size.Package, pk.ServedSize)
		}
		if !filepath.IsLocal(filepath.FromSlash(href)) {
			continue
		}
		pool := filepath.Join(repo, filepath.FromSlash(href))
		if fi, err := os.Stat(pool); err != nil {
			v.errorf("pool", pool, "listed in primary: %v", err)
		} else if fi.Size() != pk.ServedSize {
			v.errorf("pool", pool, "has %d bytes, primary says %d", fi.Size(), pk.ServedSize)
		}
	}
	for href, pk := range want {
		v.errorf("primary", file, "does not list %s-%s-%s.%s (%s)", pk.Name, pk.Version, pk.Release, pk.Arch, href)
	}
}

// history checks that the repodata files the previous snapshot listed are
// still served, so a client holding its repomd.xml can finish an update
// (design section 9, step 4).
func (v *rpmVerifier) history(rel, arch, repodata string) {
	if v.r.Previous == "" {
		return
	}
	md, err := os.ReadFile(filepath.Join(v.r.Previous, rel, arch, "repodata", "repomd.xml"))
	if err != nil {
		return // the repository is new, or the snapshot already collected
	}
	var m repomd
	if xml.Unmarshal(md, &m) != nil {
		return
	}
	for _, d := range m.Data {
		name := path.Base(d.Location.Href)
		if name == "." || name == "/" || strings.HasPrefix(name, "repomd") {
			continue
		}
		if _, err := os.Stat(filepath.Join(repodata, name)); err != nil {
			v.errorf("index-history", filepath.Join(repodata, name), "repodata of the previous snapshot is not carried over")
		}
	}
}

func checksumOK(typ, want string, data []byte) bool {
	if typ != "sha256" {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == strings.TrimSpace(want)
}

func decompress(name string, data []byte) ([]byte, error) {
	var r io.Reader
	switch {
	case strings.HasSuffix(name, ".zst"):
		zr, err := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderMaxMemory(maxMetadataFile))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	case strings.HasSuffix(name, ".gz"):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		r = zr
	case strings.HasSuffix(name, ".xz"):
		xr, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		r = xr
	default:
		return nil, fmt.Errorf("unknown compression of %s", path.Base(name))
	}
	out, err := io.ReadAll(io.LimitReader(r, maxMetadataFile+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxMetadataFile {
		return nil, errTooLarge
	}
	return out, nil
}

var errTooLarge = errors.New("larger than the verify limit")

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMetadataFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadataFile {
		return nil, errTooLarge
	}
	return data, nil
}
