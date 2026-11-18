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
func buildDeb(t *testing.T, members ...debMember) []byte {
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

func rawAr(t *testing.T, members ...debMember) []byte {
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

func controlTar(t *testing.T, name, content string) []byte {
	t.Helper()
	return tarFiles(t, name, content)
}

// tarFiles returns a tar with one regular file per name and content pair.
func tarFiles(t *testing.T, nameContent ...string) []byte {
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

func compress(t *testing.T, suffix string, data []byte) []byte {
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
