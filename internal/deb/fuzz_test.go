package deb

import (
	"bytes"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/deb/debtest"
)

func FuzzRead(f *testing.F) {
	for _, suffix := range []string{"", ".gz", ".xz", ".zst"} {
		f.Add(buildDeb(f, debMember{"control.tar" + suffix, compress(f, suffix, controlTar(f, "./control", testControl))}))
	}
	f.Add(debtest.BuildWith("tool", "1:2.0~rc1-1", "all", "x", "Replaces: a, b (<< 1)\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Read(bytes.NewReader(data))
		if err != nil {
			return
		}
		checkVersionRoundTrip(t, p.Version)
		checkParagraphRoundTrip(t, p.Control)
	})
}

func FuzzParseParagraph(f *testing.F) {
	f.Add([]byte(testControl))
	f.Add([]byte("\n\nPackage: a\nDepends: b,\n c\n\n"))
	f.Add([]byte("Package: a\nDescription: x\n .\n y\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseParagraph(data)
		if err != nil {
			return
		}
		checkParagraphRoundTrip(t, p)
	})
}

func FuzzVersion(f *testing.F) {
	f.Add("1.0", "1.0-1")
	f.Add("2:1.0-1", "1:2:3")
	f.Add("1.2~beta+dfsg-0ubuntu1~24.04", "1.2-0ubuntu1")
	f.Add("1.0~", "1.0")
	f.Fuzz(func(t *testing.T, a, b string) {
		va, errA := ParseVersion(a)
		vb, errB := ParseVersion(b)
		if errA == nil {
			checkVersionRoundTrip(t, va)
		}
		if errA != nil || errB != nil {
			return
		}
		if ab, ba := Compare(va, vb), Compare(vb, va); sign(ab) != -sign(ba) {
			t.Fatalf("Compare(%q, %q) = %d but Compare(%q, %q) = %d", a, b, ab, b, a, ba)
		}
	})
}

// checkVersionRoundTrip: the string garner writes for a version parses
// back to an equal version.
func checkVersionRoundTrip(t *testing.T, v Version) {
	t.Helper()
	if c := Compare(v, v); c != 0 {
		t.Fatalf("Compare(%q, itself) = %d", v, c)
	}
	back, err := ParseVersion(v.String())
	if err != nil {
		t.Fatalf("ParseVersion(%q) of a parsed version: %v", v, err)
	}
	if back != v {
		t.Fatalf("version %#v round-trips to %#v", v, back)
	}
}

// checkParagraphRoundTrip writes p the way packagesFile does and parses it
// again. A difference means a field value can inject or alter fields in
// the Packages index.
func checkParagraphRoundTrip(t *testing.T, p Paragraph) {
	t.Helper()
	var b strings.Builder
	for _, f := range p {
		b.WriteString(f.Name + ": " + f.Value + "\n")
	}
	back, err := ParseParagraph([]byte(b.String()))
	if err != nil {
		t.Fatalf("written paragraph does not parse: %v\n%q", err, b.String())
	}
	if len(back) != len(p) {
		t.Fatalf("paragraph with %d fields round-trips to %d:\n%q", len(p), len(back), b.String())
	}
	for i := range p {
		if back[i] != p[i] {
			t.Fatalf("field %d %#v round-trips to %#v", i, p[i], back[i])
		}
	}
}
