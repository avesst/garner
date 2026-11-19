package deb

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Limits on what Read decompresses, so a hostile upload cannot exhaust
// memory or disk. Real control archives are far smaller: md5sums of a
// large package is a few MiB.
const (
	maxControlTar  = 256 << 20 // decompressed control.tar
	maxControlFile = 1 << 20   // the control file itself
	maxZstdWindow  = 64 << 20
)

// Package is what garner needs from a .deb: its identity and its control
// paragraph, which goes into the Packages index.
type Package struct {
	Name    string
	Version Version
	Arch    string
	// Replaces holds the package names of the Replaces field.
	Replaces []string
	Control  Paragraph
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	archRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Read parses a .deb read from r: an ar archive holding debian-binary,
// control.tar[.gz|.xz|.zst] and data.tar[.*], in that order. It reads r to
// the end, so a truncated file is an error. data.tar is not decompressed.
func Read(r io.Reader) (*Package, error) {
	ar, err := newArReader(r)
	if err != nil {
		return nil, err
	}

	h, err := ar.next()
	if err != nil {
		return nil, fmt.Errorf("not a Debian package: %w", err)
	}
	if h.name != "debian-binary" {
		return nil, fmt.Errorf("not a Debian package: first member is %q, want debian-binary", h.name)
	}
	ver, err := io.ReadAll(io.LimitReader(ar, 64))
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(ver, []byte("2.")) {
		return nil, fmt.Errorf("unsupported .deb format version %q", strings.TrimSpace(string(ver)))
	}

	var control Paragraph
	haveData := false
	for {
		h, err := ar.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(h.name, "_"):
			// dpkg ignores members starting with '_'.
		case strings.HasPrefix(h.name, "control.tar"):
			if control != nil {
				return nil, errors.New("more than one control member")
			}
			if haveData {
				return nil, errors.New("control member after data member")
			}
			if control, err = readControl(ar, strings.TrimPrefix(h.name, "control.tar")); err != nil {
				return nil, err
			}
		case strings.HasPrefix(h.name, "data.tar"):
			if haveData {
				return nil, errors.New("more than one data member")
			}
			if !dataSuffixes[strings.TrimPrefix(h.name, "data.tar")] {
				return nil, fmt.Errorf("unsupported data member %q", h.name)
			}
			haveData = true
		default:
			return nil, fmt.Errorf("unexpected member %q", h.name)
		}
	}
	if control == nil {
		return nil, errors.New("no control member")
	}
	if !haveData {
		return nil, errors.New("no data member")
	}
	return newPackage(control)
}

var dataSuffixes = map[string]bool{
	"": true, ".gz": true, ".xz": true, ".zst": true, ".bz2": true, ".lzma": true,
}

func newPackage(control Paragraph) (*Package, error) {
	p := &Package{Control: control}
	var ok bool
	if p.Name, ok = control.Get("Package"); !ok {
		return nil, errors.New("control: missing Package field")
	}
	if !nameRE.MatchString(p.Name) {
		return nil, fmt.Errorf("control: invalid package name %q", p.Name)
	}
	v, ok := control.Get("Version")
	if !ok {
		return nil, errors.New("control: missing Version field")
	}
	var err error
	if p.Version, err = ParseVersion(v); err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	if p.Arch, ok = control.Get("Architecture"); !ok {
		return nil, errors.New("control: missing Architecture field")
	}
	if !archRE.MatchString(p.Arch) {
		return nil, fmt.Errorf("control: invalid architecture %q", p.Arch)
	}
	if v, ok := control.Get("Replaces"); ok {
		if p.Replaces, err = relationNames(v); err != nil {
			return nil, fmt.Errorf("control: Replaces: %w", err)
		}
	}
	return p, nil
}

// relationNames returns the package names of a relationship field such as
// "a (<< 2.0), b:any | c", without versions, architecture qualifiers or
// restrictions.
func relationNames(field string) ([]string, error) {
	var out []string
	for _, rel := range strings.FieldsFunc(field, func(r rune) bool { return r == ',' || r == '|' }) {
		name := strings.TrimSpace(rel)
		if i := strings.IndexAny(name, " \t\n(:[<"); i >= 0 {
			name = name[:i]
		}
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("invalid package name in %q", strings.TrimSpace(rel))
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// readControl decompresses a control.tar member and parses its control
// file.
func readControl(r io.Reader, suffix string) (Paragraph, error) {
	var dr io.Reader
	switch suffix {
	case "":
		dr = r
	case ".gz":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("control.tar.gz: %w", err)
		}
		defer zr.Close()
		dr = zr
	case ".xz":
		data, err := io.ReadAll(io.LimitReader(r, maxXZControl+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxXZControl {
			return nil, fmt.Errorf("control.tar.xz exceeds %d bytes", maxXZControl)
		}
		if err := checkXZ(data); err != nil {
			return nil, fmt.Errorf("control.tar.xz: %w", err)
		}
		xr, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("control.tar.xz: %w", err)
		}
		dr = xr
	case ".zst":
		zr, err := zstd.NewReader(r,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(maxZstdWindow))
		if err != nil {
			return nil, fmt.Errorf("control.tar.zst: %w", err)
		}
		defer zr.Close()
		dr = zr
	default:
		return nil, fmt.Errorf("unsupported control member control.tar%s", suffix)
	}

	// Read to the end: dpkg extracts every entry, so a later control
	// would replace the one checked here. The member names must also map
	// to files the way GNU tar, which dpkg-deb extracts with, maps them;
	// see controlMembers.
	tr := tar.NewReader(&limitedReader{r: dr, n: maxControlTar})
	var control Paragraph
	members := controlMembers{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			if err := members.check(); err != nil {
				return nil, err
			}
			if control == nil {
				return nil, errors.New("control.tar has no control file")
			}
			return control, nil
		}
		if err != nil {
			return nil, fmt.Errorf("control.tar: %w", err)
		}
		name, err := members.add(h)
		if err != nil {
			return nil, err
		}
		if name != "control" {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return nil, errors.New("control.tar: control is not a regular file")
		}
		if h.Size > maxControlFile {
			return nil, fmt.Errorf("control file is %d bytes, limit %d", h.Size, maxControlFile)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("control.tar: %w", err)
		}
		control, err = ParseParagraph(data)
		if err != nil {
			return nil, fmt.Errorf("control: %w", err)
		}
	}
}

// controlMembers tracks control.tar member names so that path.Clean of a
// name is the file GNU tar extracts it to. GNU tar strips a leading "/",
// refuses names with "..", follows symlinks it extracted earlier, and
// overwrites a file extracted earlier under the same name. dpkg-deb never
// builds any of these, so they are rejected rather than mapped.
type controlMembers struct {
	seen     map[string]bool
	names    []string
	symlinks map[string]bool
}

// add records a member and returns its cleaned name.
func (m *controlMembers) add(h *tar.Header) (string, error) {
	if strings.HasPrefix(h.Name, "/") {
		return "", fmt.Errorf("control.tar member %q is absolute", h.Name)
	}
	if slices.Contains(strings.Split(h.Name, "/"), "..") {
		return "", fmt.Errorf("control.tar member %q contains '..'", h.Name)
	}
	name := path.Clean(h.Name)
	if name == "." {
		return name, nil
	}
	if m.seen == nil {
		m.seen, m.symlinks = map[string]bool{}, map[string]bool{}
	}
	if m.seen[name] {
		if name == "control" {
			return "", errors.New("control.tar has more than one control file")
		}
		return "", fmt.Errorf("control.tar has %s more than once", name)
	}
	m.seen[name] = true
	m.names = append(m.names, name)
	if h.Typeflag == tar.TypeSymlink {
		m.symlinks[name] = true
	}
	return name, nil
}

// check rejects a member below a symlink member, in either order: tar
// would write it through the symlink, to another name.
func (m *controlMembers) check() error {
	for _, name := range m.names {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if m.symlinks[dir] {
				return fmt.Errorf("control.tar member %q is under symlink %q", name, dir)
			}
		}
	}
	return nil
}

// limitedReader is io.LimitReader with an error instead of a silent EOF,
// so an oversized archive is reported as such.
type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, fmt.Errorf("decompressed control.tar exceeds %d bytes", maxControlTar)
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// arReader reads the common ar format used by .deb files: a global header,
// then members with a 60-byte header each, padded to an even size.
type arReader struct {
	r       *bufio.Reader
	current io.Reader // unread remainder of the current member
	pad     int64
}

type arHeader struct {
	name string
	size int64
}

func newArReader(r io.Reader) (*arReader, error) {
	br := bufio.NewReader(r)
	magic := make([]byte, 8)
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != "!<arch>\n" {
		return nil, errors.New("not a Debian package: missing ar header")
	}
	return &arReader{r: br}, nil
}

// next skips the rest of the current member and returns the next header.
// It returns io.EOF at the clean end of the archive.
func (a *arReader) next() (arHeader, error) {
	if a.current != nil {
		if _, err := io.Copy(io.Discard, a.current); err != nil {
			return arHeader{}, err
		}
		if _, err := io.CopyN(io.Discard, a.r, a.pad); err != nil {
			return arHeader{}, truncated(err)
		}
	}
	buf := make([]byte, 60)
	n, err := io.ReadFull(a.r, buf)
	if n == 0 && err == io.EOF {
		return arHeader{}, io.EOF
	}
	if err != nil {
		return arHeader{}, truncated(err)
	}
	if string(buf[58:60]) != "`\n" {
		return arHeader{}, errors.New("corrupt ar member header")
	}
	name := strings.TrimRight(string(buf[0:16]), " ")
	name = strings.TrimSuffix(name, "/") // GNU ar
	size, err := strconv.ParseInt(strings.TrimRight(string(buf[48:58]), " "), 10, 64)
	if err != nil || size < 0 {
		return arHeader{}, fmt.Errorf("ar member %q: invalid size", name)
	}
	a.current = &memberReader{r: io.LimitReader(a.r, size), left: size}
	a.pad = size % 2
	return arHeader{name: name, size: size}, nil
}

func (a *arReader) Read(p []byte) (int, error) {
	if a.current == nil {
		return 0, io.EOF
	}
	return a.current.Read(p)
}

// memberReader reports a member cut short by the end of the file as
// truncation, where io.LimitReader would return a plain EOF.
type memberReader struct {
	r    io.Reader
	left int64
}

func (m *memberReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.left -= int64(n)
	if err == io.EOF && m.left > 0 {
		return n, errTruncated
	}
	return n, err
}

var errTruncated = errors.New("truncated package file")

func truncated(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || err == io.EOF {
		return errTruncated
	}
	return err
}
