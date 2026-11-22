package deb

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ulikunitz/xz"

	"github.com/avesst/garner/internal/format"
)

// maxIndexFile bounds what Verify reads of one index file into memory.
const maxIndexFile = 512 << 20

// Verify checks each configured suite like apt reads it: InRelease and
// Release.gpg against the published keys, every file Release lists by size
// and hash (also by hash), the Packages stanzas against the pool and the
// snapshot's package set, and the index files of the previous snapshot.
// It parses the files rather than reusing the renderer, so a rendering
// bug does not check itself.
func (Format) Verify(ctx context.Context, r *format.VerifyRequest) []format.Problem {
	v := &debVerifier{r: r}
	for _, suite := range r.Config.Deb.Suites {
		if ctx.Err() != nil {
			v.errorf("cancelled", r.Dir, "%v", ctx.Err())
			break
		}
		v.suite(suite)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "pool")); err != nil {
		v.errorf("pool-link", filepath.Join(r.Dir, "pool"), "snapshot pool link does not resolve: %v", err)
	}
	return v.problems
}

type debVerifier struct {
	r        *format.VerifyRequest
	problems []format.Problem
}

func (v *debVerifier) errorf(check, path, f string, args ...any) {
	v.problems = append(v.problems, format.Errorf(check, path, f, args...))
}

// releaseEntry is one line of a Release hash field.
type releaseEntry struct {
	sum  string
	size int64
	path string
}

func releaseEntries(p Paragraph, field string) ([]releaseEntry, bool) {
	val, ok := p.Get(field)
	if !ok {
		return nil, false
	}
	var out []releaseEntry
	for _, line := range strings.Split(val, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return nil, false
		}
		size, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, false
		}
		out = append(out, releaseEntry{sum: f[0], size: size, path: f[2]})
	}
	return out, true
}

func (v *debVerifier) suite(suite string) {
	dir := filepath.Join(v.r.Dir, "dists", suite)
	releasePath := filepath.Join(dir, "Release")
	release, err := os.ReadFile(releasePath)
	if err != nil {
		v.errorf("release", releasePath, "%v", err)
		return
	}

	inPath := filepath.Join(dir, "InRelease")
	if in, err := os.ReadFile(inPath); err != nil {
		v.errorf("signature", inPath, "%v", err)
	} else if text, err := v.r.Keys.VerifyClearSigned(in); err != nil {
		v.errorf("signature", inPath, "does not verify with the published keys: %v", err)
	} else if !bytes.Equal(text, release) {
		v.errorf("signature", inPath, "signs other content than Release")
	}
	gpgPath := filepath.Join(dir, "Release.gpg")
	if sig, err := os.ReadFile(gpgPath); err != nil {
		v.errorf("signature", gpgPath, "%v", err)
	} else if err := v.r.Keys.VerifyDetached(release, sig); err != nil {
		v.errorf("signature", gpgPath, "does not verify Release with the published keys: %v", err)
	}

	p, err := ParseParagraph(release)
	if err != nil {
		v.errorf("release", releasePath, "%v", err)
		return
	}
	if s, _ := p.Get("Suite"); s != suite {
		v.errorf("release", releasePath, "Suite is %q, want %q", s, suite)
	}
	if a, _ := p.Get("Architectures"); a != strings.Join(v.r.Config.Deb.Architectures, " ") {
		v.errorf("release", releasePath, "Architectures is %q, want %q", a, strings.Join(v.r.Config.Deb.Architectures, " "))
	}
	sha, ok := releaseEntries(p, "SHA256")
	if !ok {
		v.errorf("release", releasePath, "missing or malformed SHA256 field")
		return
	}
	md5s, ok := releaseEntries(p, "MD5Sum")
	if !ok {
		v.errorf("release", releasePath, "missing or malformed MD5Sum field")
	}

	listed := map[string][]byte{} // path -> content, for files that check out
	for _, e := range sha {
		full := filepath.Join(dir, filepath.FromSlash(e.path))
		if !filepath.IsLocal(filepath.FromSlash(e.path)) {
			v.errorf("release-hash", releasePath, "lists a path outside the suite: %q", e.path)
			continue
		}
		data, err := readLimited(full)
		if err != nil {
			v.errorf("release-hash", full, "listed in Release: %v", err)
			continue
		}
		if int64(len(data)) != e.size || sha256Hex(data) != e.sum {
			v.errorf("release-hash", full, "size or SHA256 differs from Release")
			continue
		}
		byHash := filepath.Join(filepath.Dir(full), "by-hash", "SHA256", e.sum)
		if bh, err := readLimited(byHash); err != nil {
			v.errorf("by-hash", byHash, "%v", err)
		} else if sha256Hex(bh) != e.sum {
			v.errorf("by-hash", byHash, "content does not match its name")
		}
		listed[e.path] = data
	}
	for _, e := range md5s {
		data, ok := listed[e.path]
		if !ok {
			continue
		}
		sum := md5.Sum(data)
		if int64(len(data)) != e.size || hex.EncodeToString(sum[:]) != e.sum {
			v.errorf("release-hash", filepath.Join(dir, filepath.FromSlash(e.path)), "size or MD5 differs from Release")
		}
	}

	for _, arch := range v.r.Config.Deb.Architectures {
		base := path.Join("main", "binary-"+arch, "Packages")
		packages, ok := listed[base]
		if !ok {
			v.errorf("release-hash", filepath.Join(dir, filepath.FromSlash(base)), "not listed in Release or does not match it")
			continue
		}
		for _, ext := range []string{".gz", ".xz"} {
			full := filepath.Join(dir, filepath.FromSlash(base+ext))
			data, ok := listed[base+ext]
			if !ok {
				v.errorf("release-hash", full, "not listed in Release or does not match it")
				continue
			}
			plain, err := decompress(ext, data)
			if err != nil {
				v.errorf("compression", full, "%v", err)
			} else if !bytes.Equal(plain, packages) {
				v.errorf("compression", full, "does not decompress to Packages")
			}
		}
		v.packages(suite, arch, filepath.Join(dir, filepath.FromSlash(base)), packages)
	}

	v.history(suite, dir)
}

// packages checks a Packages file against the packages the snapshot
// serves for suite and arch, and each stanza against its pool file.
func (v *debVerifier) packages(suite, arch, file string, data []byte) {
	want := map[string]*format.Package{}
	for _, pk := range v.r.Packages {
		if pk.Distribution == suite && (pk.Arch == arch || pk.Arch == "all") {
			want["pool/"+Format{}.PoolPath(pk)] = pk
		}
	}
	for _, stanza := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
		if stanza == "" {
			continue
		}
		p, err := ParseParagraph([]byte(stanza))
		if err != nil {
			v.errorf("packages", file, "%v", err)
			continue
		}
		name, _ := p.Get("Package")
		filename, _ := p.Get("Filename")
		pk, ok := want[filename]
		if !ok {
			v.errorf("packages", file, "lists %s (%s), which the snapshot does not serve here", name, filename)
			continue
		}
		delete(want, filename)
		if name != pk.Name {
			v.errorf("packages", file, "%s: Package is %q, want %q", filename, name, pk.Name)
		}
		if a, _ := p.Get("Architecture"); a != pk.Arch {
			v.errorf("packages", file, "%s: Architecture is %q, want %q", filename, a, pk.Arch)
		}
		if s, _ := p.Get("Version"); !sameVersion(s, pk) {
			v.errorf("packages", file, "%s: Version is %q, want %d:%s", filename, s, pk.Epoch, pk.Version)
		}
		if s, _ := p.Get("SHA256"); s != pk.ServedSHA256() {
			v.errorf("packages", file, "%s: SHA256 is %s, the snapshot serves %s", filename, s, pk.ServedSHA256())
		}
		size, _ := p.Get("Size")
		if n, err := strconv.ParseInt(size, 10, 64); err != nil || n != pk.ServedSize {
			v.errorf("packages", file, "%s: Size is %q, the served file has %d bytes", filename, size, pk.ServedSize)
		}
		pool := filepath.Join(v.r.Dir, filepath.FromSlash(filename))
		if fi, err := os.Stat(pool); err != nil {
			v.errorf("pool", pool, "listed in %s: %v", file, err)
		} else if fi.Size() != pk.ServedSize {
			v.errorf("pool", pool, "has %d bytes, Packages says %d", fi.Size(), pk.ServedSize)
		}
	}
	for filename, pk := range want {
		v.errorf("packages", file, "does not list %s %s (%s)", pk.Name, pk.Version, filename)
	}
}

func sameVersion(s string, pk *format.Package) bool {
	got, err := ParseVersion(s)
	if err != nil {
		return false
	}
	return got == version(pk)
}

// history checks that the index files the previous snapshot listed are
// still reachable by hash, so a client holding its InRelease can finish
// an update (design section 9, step 4).
func (v *debVerifier) history(suite, dir string) {
	if v.r.Previous == "" {
		return
	}
	release, err := os.ReadFile(filepath.Join(v.r.Previous, "dists", suite, "Release"))
	if err != nil {
		return // the suite is new, or the snapshot already collected
	}
	p, err := ParseParagraph(release)
	if err != nil {
		return
	}
	entries, _ := releaseEntries(p, "SHA256")
	for _, e := range entries {
		if !filepath.IsLocal(filepath.FromSlash(e.path)) {
			continue
		}
		byHash := filepath.Join(dir, filepath.FromSlash(path.Dir(e.path)), "by-hash", "SHA256", e.sum)
		if _, err := os.Stat(byHash); err != nil {
			v.errorf("index-history", byHash, "%s of the previous snapshot is not reachable by hash", e.path)
		}
	}
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxIndexFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxIndexFile {
		return nil, errTooLarge
	}
	return data, nil
}

var errTooLarge = errors.New("larger than the verify limit")

func decompress(ext string, data []byte) ([]byte, error) {
	var r io.Reader
	switch ext {
	case ".gz":
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		r = zr
	case ".xz":
		xr, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		r = xr
	}
	out, err := io.ReadAll(io.LimitReader(r, maxIndexFile+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxIndexFile {
		return nil, errTooLarge
	}
	return out, nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
