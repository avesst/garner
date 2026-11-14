package rpm

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestReadHeader(t *testing.T) {
	tests := []struct {
		file string
		want Package
	}{
		{"testdata/garner-hello-1.0-1.noarch.rpm", Package{Name: "garner-hello", Epoch: 0, Version: "1.0",
			Release: "1", Arch: "noarch", Summary: "garner test package", SourceRPM: "garner-hello-1.0-1.src.rpm"}},
		{"testdata/garner-hello-2.0-1.x86_64.rpm", Package{Name: "garner-hello", Epoch: 3, Version: "2.0",
			Release: "1", Arch: "x86_64", Summary: "garner test package", SourceRPM: "garner-hello-2.0-1.src.rpm"}},
	}
	for _, tt := range tests {
		f, err := os.Open(tt.file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadHeader(f)
		f.Close()
		if err != nil {
			t.Errorf("%s: %v", tt.file, err)
			continue
		}
		if *got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.file, *got, tt.want)
		}
	}
}

func TestReadHeaderInvalid(t *testing.T) {
	good, err := os.ReadFile("testdata/garner-hello-1.0-1.noarch.rpm")
	if err != nil {
		t.Fatal(err)
	}
	source := bytes.Clone(good)
	source[7] = 1 // lead type: source package

	huge := bytes.Clone(good)
	copy(huge[96+8:96+16], []byte{0x7f, 0xff, 0xff, 0xff, 0x7f, 0xff, 0xff, 0xff})

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "missing lead"},
		{"deb", []byte("!<arch>\ndebian-binary   "), "missing lead"},
		{"truncated lead", good[:50], "missing lead"},
		{"truncated header", good[:200], "truncated"},
		{"source rpm", source, "source rpms"},
		{"huge header", huge, "too large"},
	}
	for _, tt := range tests {
		_, err := ReadHeader(bytes.NewReader(tt.data))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}
