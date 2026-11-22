package rpm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func FuzzReadHeader(f *testing.F) {
	files, err := filepath.Glob("testdata/*.rpm")
	if err != nil {
		f.Fatal(err)
	}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add(fakeRPM(map[int32]string{tagName: "a", tagVersion: "1", tagRelease: "1", tagArch: "noarch"}))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ReadHeader(bytes.NewReader(data))
		if err != nil {
			return
		}
		for _, s := range []string{p.Name, p.Version, p.Release, p.Arch} {
			if s == "" {
				t.Fatalf("accepted package with an empty identity field: %+v", p)
			}
		}
		if p.Epoch < 0 {
			t.Fatalf("accepted negative epoch %d", p.Epoch)
		}
	})
}

func FuzzVercmp(f *testing.F) {
	f.Add("1.0", "1.0.1")
	f.Add("1.0~rc1", "1.0")
	f.Add("1.0^git1", "1.0")
	f.Add("1.0a", "1.0.a")
	f.Add("010", "10")
	f.Fuzz(func(t *testing.T, a, b string) {
		if c := Vercmp(a, a); c != 0 {
			t.Fatalf("Vercmp(%q, itself) = %d", a, c)
		}
		ab, ba := Vercmp(a, b), Vercmp(b, a)
		if ab != -ba {
			t.Fatalf("Vercmp(%q, %q) = %d but Vercmp(%q, %q) = %d", a, b, ab, b, a, ba)
		}
		if ab < -1 || ab > 1 {
			t.Fatalf("Vercmp(%q, %q) = %d, want -1, 0 or 1", a, b, ab)
		}
	})
}
