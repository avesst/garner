package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const testControl = `Package: tool
Version: 1:1.4.0-1
Architecture: amd64
Maintainer: Example <dev@example.com>
Installed-Size: 12
Description: a test package
 It has a long description.
 .
 With a blank line.
`

func TestReadCompressions(t *testing.T) {
	for _, suffix := range []string{"", ".gz", ".xz", ".zst"} {
		t.Run("control.tar"+suffix, func(t *testing.T) {
			deb := buildDeb(t, debMember{"control.tar" + suffix, compress(t, suffix, controlTar(t, "./control", testControl))})
			p, err := Read(bytes.NewReader(deb))
			if err != nil {
				t.Fatal(err)
			}
			checkTestPackage(t, p)
		})
	}
}

func checkTestPackage(t *testing.T, p *Package) {
	t.Helper()
	if p.Name != "tool" || p.Version != (Version{1, "1.4.0", "1"}) || p.Arch != "amd64" {
		t.Errorf("got %s %s %s, want tool 1:1.4.0-1 amd64", p.Name, p.Version, p.Arch)
	}
	desc, _ := p.Control.Get("description")
	if want := "a test package\n It has a long description.\n .\n With a blank line."; desc != want {
		t.Errorf("Description = %q, want %q", desc, want)
	}
	if len(p.Control) != 6 || p.Control[3].Name != "Maintainer" {
		t.Errorf("fields not kept in order: %+v", p.Control)
	}
}

// TestReadDpkgDebBuilt parses packages built by the real dpkg-deb, with each
// compression it supports. It is skipped where dpkg-deb is not installed.
func TestReadDpkgDebBuilt(t *testing.T) {
	dpkgDeb, err := exec.LookPath("dpkg-deb")
	if err != nil {
		t.Skip("dpkg-deb not installed")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	for _, dir := range []string{"DEBIAN", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "DEBIAN/control"), []byte(testControl), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "usr/bin/tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// dpkg-deb archives symlinked maintainer scripts and subdirectories
	// of DEBIAN as they are; they must stay accepted.
	if err := os.WriteFile(filepath.Join(src, "DEBIAN/postinst"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("postinst", filepath.Join(src, "DEBIAN/prerm")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "DEBIAN/extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "DEBIAN/extra/notes"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, z := range []string{"none", "gzip", "xz", "zstd"} {
		t.Run(z, func(t *testing.T) {
			out := filepath.Join(root, z+".deb")
			cmd := exec.Command(dpkgDeb, "--root-owner-group", "-Z"+z, "--build", src, out)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("dpkg-deb: %v\n%s", err, b)
			}
			f, err := os.Open(out)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			p, err := Read(f)
			if err != nil {
				t.Fatal(err)
			}
			checkTestPackage(t, p)
		})
	}
}

func TestReadInvalid(t *testing.T) {
	good := controlTar(t, "./control", testControl)
	withControl := func(control string) []byte {
		return buildDeb(t, debMember{"control.tar", controlTar(t, "./control", control)})
	}
	valid := buildDeb(t, debMember{"control.tar", good})

	tests := []struct {
		name string
		deb  []byte
		want string
	}{
		{"empty", nil, "missing ar header"},
		{"not ar", []byte("PK\x03\x04 a zip file"), "missing ar header"},
		{"truncated", valid[:len(valid)-10], "truncated"},
		{"cut in header", valid[:8+30], "truncated"},
		{"wrong first member", rawAr(t, debMember{"control.tar", good}), "want debian-binary"},
		{"format 3", rawAr(t, debMember{"debian-binary", []byte("3.0\n")}), "format version"},
		{"no control", rawAr(t, debMember{"debian-binary", []byte("2.0\n")}, debMember{"data.tar", nil}), "no control member"},
		{"no data", rawAr(t, debMember{"debian-binary", []byte("2.0\n")}, debMember{"control.tar", good}), "no data member"},
		{"data first", rawAr(t, debMember{"debian-binary", []byte("2.0\n")}, debMember{"data.tar", nil}, debMember{"control.tar", good}), "after data"},
		{"unknown member", buildDeb(t, debMember{"control.tar", good}, debMember{"extra", nil}), "unexpected member"},
		{"bzip2 control", buildDeb(t, debMember{"control.tar.bz2", good}), "unsupported control"},
		{"no control file", buildDeb(t, debMember{"control.tar", controlTar(t, "./md5sums", "")}), "no control file"},
		// dpkg extracts the whole archive, so the last control wins; garner
		// would read the first and check ownership of the wrong name.
		{"two control files", buildDeb(t, debMember{"control.tar", tarFiles(t,
			"./control", testControl,
			"control", strings.Replace(testControl, "Package: tool", "Package: other", 1))}), "more than one control file"},
		{"corrupt gzip", buildDeb(t, debMember{"control.tar.gz", []byte("not gzip")}), "control.tar.gz"},
		{"missing version", withControl("Package: tool\nArchitecture: amd64\n"), "missing Version"},
		{"bad version", withControl("Package: tool\nVersion: v1\nArchitecture: amd64\n"), "start with a digit"},
		{"bad name", withControl("Package: Tool\nVersion: 1\nArchitecture: amd64\n"), "invalid package name"},
		{"bad arch", withControl("Package: tool\nVersion: 1\nArchitecture: amd 64\n"), "invalid architecture"},
		{"two paragraphs", withControl("Package: tool\n\nPackage: other\n"), "more than one paragraph"},
		{"duplicate field", withControl("Package: tool\npackage: tool\n"), "duplicate field"},
		{"no colon", withControl("Package tool\n"), "missing ':'"},
		{"bad replaces", withControl("Package: tool\nVersion: 1\nArchitecture: amd64\nReplaces: Old Tool\n"), "Replaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Read(bytes.NewReader(tt.deb))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestReadControlAliases covers control.tar members that GNU tar, which
// dpkg-deb extracts with, writes to ./control although their names do not
// clean to "control". dpkg would install the second control file while
// garner checked the first.
func TestReadControlAliases(t *testing.T) {
	other := strings.Replace(testControl, "Package: tool", "Package: other", 1)
	reg := func(name, content string) tar.Header {
		return tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(content))}
	}
	sym := func(name, target string) tar.Header {
		return tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777}
	}
	tests := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{"absolute", []tarEntry{{reg("./control", testControl), testControl}, {reg("/control", other), other}}, "absolute"},
		{"double slash", []tarEntry{{reg("./control", testControl), testControl}, {reg("//control", other), other}}, "absolute"},
		{"slash dot", []tarEntry{{reg("./control", testControl), testControl}, {reg("/./control", other), other}}, "absolute"},
		{"symlinked dir", []tarEntry{{reg("./control", testControl), testControl}, {sym("./d", "."), ""}, {reg("./d/control", other), other}}, "under symlink"},
		{"symlinked dir, unclean", []tarEntry{{reg("./control", testControl), testControl}, {sym("d", "."), ""}, {reg("d/./control", other), other}}, "under symlink"},
	}
	dpkgDeb, _ := exec.LookPath("dpkg-deb")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deb := buildDeb(t, debMember{"control.tar", tarEntries(t, tt.entries...)})
			// Confirm the case is real: dpkg-deb reads a different
			// control file than the one named "./control".
			if dpkgDeb != "" {
				f := filepath.Join(t.TempDir(), "x.deb")
				if err := os.WriteFile(f, deb, 0o644); err != nil {
					t.Fatal(err)
				}
				out, err := exec.Command(dpkgDeb, "-f", f, "Package").Output()
				if err != nil {
					t.Fatalf("dpkg-deb -f: %v", err)
				}
				if got := strings.TrimSpace(string(out)); got != "other" {
					t.Fatalf("dpkg-deb reads Package %q, want other: the case does not alias", got)
				}
			} else {
				t.Log("dpkg-deb not installed: not compared")
			}
			_, err := Read(bytes.NewReader(deb))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestReadControlMemberNames(t *testing.T) {
	reg := func(name string) tarEntry {
		return tarEntry{tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(testControl))}, testControl}
	}
	tests := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{"dot dot", []tarEntry{reg("../control")}, "'..'"},
		{"inner dot dot", []tarEntry{reg("./control"), reg("a/../../x")}, "'..'"},
		{"duplicate", []tarEntry{reg("./control"), reg("./md5sums"), reg("md5sums")}, "more than once"},
		// Rejected whatever the order, though here the later ./control
		// overwrites what was written through the symlink.
		{"under symlink before control", []tarEntry{
			{tar.Header{Typeflag: tar.TypeSymlink, Name: "./d", Linkname: ".", Mode: 0o777}, ""},
			reg("./d/control"), reg("./control")}, "under symlink"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Read(bytes.NewReader(buildDeb(t, debMember{"control.tar", tarEntries(t, tt.entries...)})))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A member name with many components compresses to a few bytes, so the
// time to check member names must not grow with the square of its length.
func TestReadDeepMemberName(t *testing.T) {
	name := strings.Repeat("a/", 50000) + "x"
	deb := buildDeb(t, debMember{"control.tar.gz", compress(t, ".gz", tarFiles(t, "./control", testControl, name, ""))})
	start := time.Now()
	_, err := Read(bytes.NewReader(deb))
	if d := time.Since(start); d > time.Second {
		t.Errorf("reading a %d byte deb took %v (err = %v)", len(deb), d, err)
	}
}

// Names within the length limit still have many components each, and a
// control.tar of a few hundred KiB can hold thousands of them.
func TestReadManyDeepMemberNames(t *testing.T) {
	files := []string{"./control", testControl}
	deep := strings.Repeat("a/", 2040)
	for i := range 2000 {
		files = append(files, fmt.Sprintf("%s%d", deep, i), "")
	}
	deb := buildDeb(t, debMember{"control.tar.gz", compress(t, ".gz", tarFiles(t, files...))})
	start := time.Now()
	_, err := Read(bytes.NewReader(deb))
	if d := time.Since(start); d > time.Second {
		t.Errorf("reading a %d byte deb took %v (err = %v)", len(deb), d, err)
	}
}

func TestReadReplaces(t *testing.T) {
	control := strings.Replace(testControl, "Installed-Size: 12\n",
		"Installed-Size: 12\nReplaces: old-tool (<< 1.4), tool-data:any,\n tool-doc [amd64], old-tool\n", 1)
	p, err := Read(bytes.NewReader(buildDeb(t, debMember{"control.tar", controlTar(t, "./control", control)})))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"old-tool", "tool-data", "tool-doc"}; !slices.Equal(p.Replaces, want) {
		t.Errorf("Replaces = %q, want %q", p.Replaces, want)
	}
}

func TestReadIgnoresUnderscoreMembers(t *testing.T) {
	deb := buildDeb(t, debMember{"_gpgorigin", []byte("sig")}, debMember{"control.tar", controlTar(t, "control", testControl)})
	if _, err := Read(bytes.NewReader(deb)); err != nil {
		t.Fatal(err)
	}
}

func TestReadControlTarLimit(t *testing.T) {
	// A small gzip stream that inflates past the limit before the control
	// file appears.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)
	const size = maxControlTar + 1
	if err := tw.WriteHeader(&tar.Header{Name: "./md5sums", Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zeros{}, size); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()
	deb := buildDeb(t, debMember{"control.tar.gz", gz.Bytes()})
	if _, err := Read(bytes.NewReader(deb)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want a size limit error", err)
	}
}

func TestReadXZDictLimit(t *testing.T) {
	good := compress(t, ".xz", controlTar(t, "./control", testControl))
	huge := xzDict(t, good, 38) // 2 GiB, which the decoder would allocate
	tests := []struct {
		name string
		data []byte
		want string // empty: accepted
	}{
		{"64 MiB", xzDict(t, good, 28), ""},
		{"2 GiB", huge, "dictionary"},
		{"4 GiB", xzDict(t, good, 40), "dictionary"},
		// The walk must reach headers after the first one.
		{"second stream", append(append(bytes.Clone(good), 0, 0, 0, 0), huge...), "dictionary"},
		{"truncated", good[:len(good)-5], "truncated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Read(bytes.NewReader(buildDeb(t, debMember{"control.tar.xz", tt.data})))
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// xzDict returns a copy of the single-block xz stream data with the
// dictionary size property of its block header set to p, and the header
// CRC32 fixed so the decoder would accept it.
func xzDict(t *testing.T, data []byte, p byte) []byte {
	t.Helper()
	x := bytes.Clone(data)
	size := (int(x[12]) + 1) * 4
	h := x[12 : 12+size]
	i := bytes.Index(h, []byte{0x21, 0x01})
	if i < 0 {
		t.Fatal("no LZMA2 filter in block header")
	}
	h[i+2] = p
	binary.LittleEndian.PutUint32(h[size-4:], crc32.ChecksumIEEE(h[:size-4]))
	return x
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type debMember struct {
	name string
	data []byte
}

// buildDeb returns a .deb with debian-binary, the given members, and an
// empty data.tar unless the members include a data member.
func buildDeb(t testing.TB, members ...debMember) []byte {
	t.Helper()
	all := []debMember{{"debian-binary", []byte("2.0\n")}}
	all = append(all, members...)
	hasData := false
	for _, m := range members {
		hasData = hasData || strings.HasPrefix(m.name, "data.tar")
	}
	if !hasData {
		all = append(all, debMember{"data.tar", controlTar(t, "./usr/bin/tool", "x")})
	}
	return rawAr(t, all...)
}

func rawAr(t testing.TB, members ...debMember) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		b.Write(m.data)
		if len(m.data)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

func controlTar(t testing.TB, name, content string) []byte {
	t.Helper()
	return tarFiles(t, name, content)
}

// tarFiles returns a tar with one regular file per name and content pair.
func tarFiles(t testing.TB, nameContent ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := 0; i+1 < len(nameContent); i += 2 {
		name, content := nameContent[i], nameContent[i+1]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type tarEntry struct {
	hdr     tar.Header
	content string
}

// tarEntries returns a tar with the given headers, written as they are.
func tarEntries(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		if err := tw.WriteHeader(&e.hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func compress(t testing.TB, suffix string, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser
	var err error
	switch suffix {
	case "":
		return data
	case ".gz":
		w = gzip.NewWriter(&b)
	case ".xz":
		w, err = xz.NewWriter(&b)
	case ".zst":
		w, err = zstd.NewWriter(&b)
	default:
		t.Fatalf("unknown compression %q", suffix)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
