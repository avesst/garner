package rpm

import (
	"bytes"
	"encoding/binary"
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

func TestReadHeaderFieldCharacters(t *testing.T) {
	valid := map[int32]string{tagName: "garner-hello", tagVersion: "1.0", tagRelease: "1",
		tagArch: "noarch", tagSourceRPM: "garner-hello-1.0-1.src.rpm"}
	tests := []struct {
		name  string
		tag   int32
		value string
		want  string // empty: accepted
	}{
		{"valid", tagName, "garner-hello", ""},
		{"name with plus and underscore", tagName, "_lib+plus.x", ""},
		{"version with tilde and caret", tagVersion, "1.0~rc1^git2", ""},
		{"release with dist tag", tagRelease, "1.el9_3", ""},
		{"name with slash", tagName, "../../../tmp/evil", "invalid package name"},
		{"name starting with dot", tagName, ".hidden", "invalid package name"},
		{"name with space", tagName, "a b", "invalid package name"},
		{"name with macro", tagName, "%{name}", "invalid package name"},
		{"version with slash", tagVersion, "1/../../x", "invalid version"},
		{"version with dash", tagVersion, "1.0-1", "invalid version"},
		{"version with dot dot", tagVersion, "1..0", "invalid version"},
		{"release with newline", tagRelease, "1\nx", "invalid release"},
		{"release with slash", tagRelease, "1/x", "invalid release"},
		{"arch with slash", tagArch, "../x", "invalid architecture"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tags := map[int32]string{}
			for k, v := range valid {
				tags[k] = v
			}
			tags[tt.tag] = tt.value
			_, err := ReadHeader(bytes.NewReader(fakeRPM(tags)))
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// fakeRPM returns a binary rpm with an empty signature header and a main
// header holding the given string tags.
func fakeRPM(tags map[int32]string) []byte {
	var b bytes.Buffer
	lead := make([]byte, 96)
	copy(lead, leadMagic)
	b.Write(lead)
	header := func(index, data []byte) {
		b.Write(headerMagic)
		b.Write(make([]byte, 4))
		binary.Write(&b, binary.BigEndian, uint32(len(index)/16))
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.Write(index)
		b.Write(data)
	}
	header(nil, nil) // 16 bytes, so no padding follows
	var index, data []byte
	for tag, v := range tags {
		index = binary.BigEndian.AppendUint32(index, uint32(tag))
		index = binary.BigEndian.AppendUint32(index, typeString)
		index = binary.BigEndian.AppendUint32(index, uint32(len(data)))
		index = binary.BigEndian.AppendUint32(index, 1)
		data = append(append(data, v...), 0)
	}
	header(index, data)
	return b.Bytes()
}
