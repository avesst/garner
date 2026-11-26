package rpm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
		if !reflect.DeepEqual(*got, tt.want) {
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
		{"obsoletes not an array", fakeRPM(map[int32]string{tagName: "a", tagVersion: "1", tagRelease: "1",
			tagArch: "noarch", tagSourceRPM: "a-1-1.src.rpm", tagObsoletes: "b"}), "want a string array"},
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

// A tag that appears twice could be read one way by garner and another by
// rpm, which picks a copy by binary search, so the name garner checks for
// ownership could differ from the one in the signed repodata.
func TestReadHeaderDuplicateTag(t *testing.T) {
	entries := []fakeEntry{
		{tagName, typeString, "bash"},
		{tagName, typeString, "foo"},
		{tagVersion, typeString, "1.0"},
		{tagRelease, typeString, "1"},
		{tagArch, typeString, "noarch"},
		{tagSourceRPM, typeString, "foo-1.0-1.src.rpm"},
	}
	p, err := ReadHeader(bytes.NewReader(fakeRPMEntries(entries)))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("got %+v, err = %v, want a duplicate tag error", p, err)
	}
}

// Identical copies of a tag cannot disagree, so they are accepted: some
// AlmaLinux 10 builds repeat SOURCERPM.
func TestReadHeaderIdenticalDuplicateTag(t *testing.T) {
	entries := []fakeEntry{
		{tagName, typeString, "libusb1"},
		{tagVersion, typeString, "1.0.30"},
		{tagRelease, typeString, "1.el10_2"},
		{tagEpoch, typeInt32, "0"},
		{tagEpoch, typeInt32, "0"},
		{tagArch, typeString, "x86_64"},
		{tagSourceRPM, typeString, "libusb1-1.0.30-1.el10_2.src.rpm"},
		{tagSourceRPM, typeString, "libusb1-1.0.30-1.el10_2.src.rpm"},
	}
	p, err := ReadHeader(bytes.NewReader(fakeRPMEntries(entries)))
	if err != nil {
		t.Fatal(err)
	}
	if p.SourceRPM != "libusb1-1.0.30-1.el10_2.src.rpm" {
		t.Errorf("SourceRPM = %q", p.SourceRPM)
	}

	entries[4].value = "1"
	if _, err := ReadHeader(bytes.NewReader(fakeRPMEntries(entries))); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("epoch copies 0 and 1: err = %v, want a duplicate tag error", err)
	}
}

// The tags that identify a package must have the type rpmbuild writes:
// rpm picks an I18N string by locale, so garner could read another value.
func TestReadHeaderTagTypes(t *testing.T) {
	tests := []struct {
		name  string
		entry fakeEntry
		want  string // empty: accepted
	}{
		{"summary as i18n string", fakeEntry{tagSummary, typeI18NString, "hello"}, ""},
		{"epoch as int32", fakeEntry{tagEpoch, typeInt32, "5"}, ""},
		{"name as i18n string", fakeEntry{tagName, typeI18NString, "foo"}, "type"},
		{"name as string array", fakeEntry{tagName, typeStringArray, "foo"}, "type"},
		{"arch as i18n string", fakeEntry{tagArch, typeI18NString, "noarch"}, "type"},
		{"epoch as string", fakeEntry{tagEpoch, typeString, "5"}, "type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := []fakeEntry{
				{tagName, typeString, "foo"},
				{tagVersion, typeString, "1.0"},
				{tagRelease, typeString, "1"},
				{tagArch, typeString, "noarch"},
				{tagSourceRPM, typeString, "foo-1.0-1.src.rpm"},
			}
			replaced := false
			for i := range entries {
				if entries[i].tag == tt.entry.tag {
					entries[i], replaced = tt.entry, true
				}
			}
			if !replaced {
				entries = append(entries, tt.entry)
			}
			p, err := ReadHeader(bytes.NewReader(fakeRPMEntries(entries)))
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("got %+v, err = %v, want it to contain %q", p, err, tt.want)
			}
		})
	}
}

// fakeRPM returns a binary rpm with an empty signature header and a main
// header holding the given string tags.
func fakeRPM(tags map[int32]string) []byte {
	var entries []fakeEntry
	for tag, v := range tags {
		entries = append(entries, fakeEntry{tag, typeString, v})
	}
	return fakeRPMEntries(entries)
}

// fakeEntry is one main-header index entry with a single value: a string,
// or for typeInt32 a decimal number. A string array's value holds its
// strings separated by NUL.
type fakeEntry struct {
	tag   int32
	typ   uint32
	value string
}

// fakeRPMEntries is fakeRPM with the main header index given in order.
func fakeRPMEntries(entries []fakeEntry) []byte {
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
	for _, e := range entries {
		if e.typ == typeInt32 {
			for len(data)%4 != 0 {
				data = append(data, 0)
			}
		}
		index = binary.BigEndian.AppendUint32(index, uint32(e.tag))
		index = binary.BigEndian.AppendUint32(index, e.typ)
		index = binary.BigEndian.AppendUint32(index, uint32(len(data)))
		count := uint32(1)
		if e.typ == typeStringArray || e.typ == typeI18NString {
			count += uint32(strings.Count(e.value, "\x00"))
		}
		index = binary.BigEndian.AppendUint32(index, count)
		if e.typ == typeInt32 {
			n, err := strconv.ParseInt(e.value, 10, 32)
			if err != nil {
				panic(err)
			}
			data = binary.BigEndian.AppendUint32(data, uint32(n))
		} else {
			data = append(append(data, e.value...), 0)
		}
	}
	header(index, data)
	return b.Bytes()
}

// Obsoletes become Replaces; Provides do not. Expected names are from
// rpm -qp --obsoletes on the fixture.
func TestParseObsoletes(t *testing.T) {
	for file, want := range map[string][]string{
		"testdata/garner-renamed-1.0-1.noarch.rpm": {"garner-hello", "garner-old"},
		"testdata/garner-hello-1.0-1.noarch.rpm":   nil,
	} {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		p, err := (&Format{}).Parse(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if !slices.Equal(p.Replaces, want) {
			t.Errorf("%s: Replaces = %q, want %q", file, p.Replaces, want)
		}
	}
}

// fakeRPMObsoletes is fakeRPM with an Obsoletes array of count entries
// stored in obs.
func fakeRPMObsoletes(obs []byte, count uint32) []byte {
	b := fakeRPM(map[int32]string{tagName: "a", tagVersion: "1", tagRelease: "1",
		tagArch: "noarch", tagSourceRPM: "a-1-1.src.rpm"})
	// Rebuild the main header with one more entry pointing past its data.
	main := b[96+16:]
	nindex := binary.BigEndian.Uint32(main[8:12])
	hsize := binary.BigEndian.Uint32(main[12:16])
	index, data := main[16:16+nindex*16], main[16+nindex*16:]
	var out bytes.Buffer
	out.Write(b[:96+16])
	out.Write(headerMagic)
	out.Write(make([]byte, 4))
	binary.Write(&out, binary.BigEndian, nindex+1)
	binary.Write(&out, binary.BigEndian, hsize+uint32(len(obs)))
	out.Write(index)
	for _, v := range []uint32{tagObsoletes, typeStringArray, hsize, count} {
		binary.Write(&out, binary.BigEndian, v)
	}
	out.Write(data)
	out.Write(obs)
	return out.Bytes()
}

// A header is bounded by maxHeader, but what garner builds from it must
// be too: an Obsoletes array of empty strings costs a byte each in the
// file and far more as Go strings.
func TestReadHeaderObsoletesMemory(t *testing.T) {
	const n = 32 << 20
	in := fakeRPMObsoletes(make([]byte, n), n)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := ReadHeader(bytes.NewReader(in))
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 3*uint64(len(in)) {
		t.Errorf("reading a %d MiB rpm allocated %d MiB", len(in)>>20, alloc>>20)
	}
	if err == nil || !strings.Contains(err.Error(), "obsoletes") {
		t.Errorf("err = %v, want too many obsoletes", err)
	}
}

// Obsoletes names are de-duplicated in time linear in their number.
func TestReadHeaderManyObsoletes(t *testing.T) {
	var obs []byte
	for i := range maxObsoletes {
		obs = fmt.Appendf(obs, "n%d\x00", i)
	}
	start := time.Now()
	p, err := ReadHeader(bytes.NewReader(fakeRPMObsoletes(obs, maxObsoletes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Obsoletes) != maxObsoletes {
		t.Errorf("got %d obsoletes, want %d", len(p.Obsoletes), maxObsoletes)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("reading %d obsoletes took %v", maxObsoletes, d)
	}
}

// createrepo_c fails the whole repository over a control character in
// any text it writes to repodata, so ReadHeader rejects them all.
func TestReadHeaderControlCharacters(t *testing.T) {
	tests := []struct {
		name  string
		entry fakeEntry
		want  string // empty: accepted
	}{
		{"escape in summary", fakeEntry{tagSummary, typeString, "colour \x1b[32mgreen"}, "summary: control character 0x1b"},
		{"escape in i18n summary", fakeEntry{tagSummary, typeI18NString, "bell\x07"}, "summary: control character 0x07"},
		{"escape in description", fakeEntry{1005, typeString, "a\x01b"}, "description: control character 0x01"},
		{"second changelog entry", fakeEntry{1082, typeStringArray, "- fine\x00- \x1b[1mbold"}, "changelog text: control character 0x1b"},
		{"unnamed tag", fakeEntry{5000, typeString, "x\x1fy"}, "tag 5000: control character 0x1f"},
		{"file name", fakeEntry{1117, typeStringArray, "ok\x00bad\nname\x00tab\tok"}, ""},
		{"tab, LF and CR", fakeEntry{1005, typeString, "line one\r\n\tline two\n"}, ""},
		{"DEL and UTF-8", fakeEntry{1005, typeString, "café \x7f"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := []fakeEntry{
				{tagName, typeString, "foo"},
				{tagVersion, typeString, "1.0"},
				{tagRelease, typeString, "1"},
				{tagArch, typeString, "noarch"},
				{tagSourceRPM, typeString, "foo-1.0-1.src.rpm"},
			}
			replaced := false
			for i := range entries {
				if entries[i].tag == tt.entry.tag {
					entries[i], replaced = tt.entry, true
				}
			}
			if !replaced {
				entries = append(entries, tt.entry)
			}
			_, err := ReadHeader(bytes.NewReader(fakeRPMEntries(entries)))
			if tt.want == "" && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// Index entries that all point at one long string must not make the text
// check scan it once per entry.
func TestReadHeaderOverlappingStrings(t *testing.T) {
	const entries = 200_000
	str := append(bytes.Repeat([]byte("a"), 64<<10), 0)
	var index []byte
	for i := range entries {
		index = binary.BigEndian.AppendUint32(index, uint32(5000+i))
		index = binary.BigEndian.AppendUint32(index, typeString)
		index = binary.BigEndian.AppendUint32(index, 0)
		index = binary.BigEndian.AppendUint32(index, 1)
	}
	var b bytes.Buffer
	lead := make([]byte, 96)
	copy(lead, leadMagic)
	b.Write(lead)
	b.Write(headerMagic)
	b.Write(make([]byte, 12)) // empty signature header
	b.Write(headerMagic)
	b.Write(make([]byte, 4))
	binary.Write(&b, binary.BigEndian, uint32(entries))
	binary.Write(&b, binary.BigEndian, uint32(len(str)))
	b.Write(index)
	b.Write(str)
	start := time.Now()
	_, err := ReadHeader(bytes.NewReader(b.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Errorf("err = %v, want it to mention the overlap", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
}
