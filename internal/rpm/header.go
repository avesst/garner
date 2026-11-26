package rpm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
)

// Header tags garner reads.
const (
	tagName      = 1000
	tagVersion   = 1001
	tagRelease   = 1002
	tagEpoch     = 1003
	tagSummary   = 1004
	tagArch      = 1022
	tagSourceRPM = 1044
	tagObsoletes = 1090 // OBSOLETENAME
)

// Header value types.
const (
	typeInt32       = 4
	typeString      = 6
	typeStringArray = 8
	typeI18NString  = 9
)

var (
	leadMagic   = []byte{0xed, 0xab, 0xee, 0xdb}
	headerMagic = []byte{0x8e, 0xad, 0xe8, 0x01}
)

// maxHeader bounds a header's index and data, so a hostile file cannot
// make garner allocate without limit. Real headers with large file lists
// are tens of MiB.
const maxHeader = 128 << 20

// maxObsoletes bounds the Obsoletes array, which costs a byte per entry in
// the file and far more once decoded. Real packages list at most a few
// hundred.
const maxObsoletes = 1 << 16

// The characters rpmbuild accepts in these fields, without its macro
// characters %{}: a built package has its macros expanded, and repodata
// would read % as a URL escape. Without '/', a field cannot climb out of
// the pool directory when it becomes part of a pool path.
var (
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]*$`)
	verRelRE = regexp.MustCompile(`^[A-Za-z0-9._+~^]+$`)
	archRE   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// Package is what garner reads from an rpm header.
type Package struct {
	Name      string
	Epoch     int
	Version   string
	Release   string
	Arch      string
	Summary   string
	SourceRPM string
	// Obsoletes holds the obsoleted names that can be package names.
	Obsoletes []string
}

// ReadHeader reads the lead, the signature header and the main header of
// an rpm. It rejects source packages.
func ReadHeader(r io.Reader) (*Package, error) {
	lead := make([]byte, 96)
	if _, err := io.ReadFull(r, lead); err != nil || !bytes.Equal(lead[:4], leadMagic) {
		return nil, errors.New("not an rpm package: missing lead")
	}
	if binary.BigEndian.Uint16(lead[6:8]) == 1 {
		return nil, errors.New("source rpms are not accepted")
	}
	sigLen, _, err := readHeader(r, false)
	if err != nil {
		return nil, fmt.Errorf("signature header: %w", err)
	}
	// The signature header is padded to a multiple of 8 bytes.
	if pad := (8 - sigLen%8) % 8; pad > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(pad)); err != nil {
			return nil, errors.New("truncated rpm")
		}
	}
	_, tags, err := readHeader(r, true)
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	p := &Package{
		Name:      tags.str(tagName),
		Version:   tags.str(tagVersion),
		Release:   tags.str(tagRelease),
		Arch:      tags.str(tagArch),
		Summary:   tags.str(tagSummary),
		SourceRPM: tags.str(tagSourceRPM),
	}
	if e, ok := tags.int(tagEpoch); ok {
		p.Epoch = e
	}
	// A file path or capability such as perl(Foo) cannot be the name of a
	// package garner serves, so it needs no owner.
	seen := map[string]bool{}
	for _, name := range tags.strs(tagObsoletes) {
		if nameRE.MatchString(name) && !seen[name] {
			seen[name] = true
			p.Obsoletes = append(p.Obsoletes, name)
		}
	}
	switch {
	case p.Name == "" || p.Version == "" || p.Release == "" || p.Arch == "":
		return nil, errors.New("rpm header lacks name, version, release or arch")
	case !nameRE.MatchString(p.Name):
		return nil, fmt.Errorf("invalid package name %q", p.Name)
	case !verRelRE.MatchString(p.Version) || strings.Contains(p.Version, ".."):
		return nil, fmt.Errorf("invalid version %q", p.Version)
	case !verRelRE.MatchString(p.Release) || strings.Contains(p.Release, ".."):
		return nil, fmt.Errorf("invalid release %q", p.Release)
	case !archRE.MatchString(p.Arch):
		return nil, fmt.Errorf("invalid architecture %q", p.Arch)
	case p.SourceRPM == "":
		return nil, errors.New("source rpms are not accepted")
	case p.Epoch < 0:
		return nil, errors.New("negative epoch")
	}
	return p, nil
}

// textCheck rejects control characters in the text of a main header.
// createrepo_c copies summaries, descriptions, changelogs, file names,
// dependencies and the like into the repodata XML, where bytes below 0x20
// other than tab, LF and CR are not allowed. createrepo_c 1.1.2 (EL 10)
// and 1.2.0 (Debian 13) then fail the whole repository, so one such
// package would stop every publish. garner rejects the package rather
// than strip the bytes (design section 7).
type textCheck struct {
	data    []byte
	scanned int
}

// textTagNames name the tags a control character most likely sits in.
var textTagNames = map[int32]string{
	1004: "summary", 1005: "description", 1011: "vendor", 1014: "license",
	1015: "packager", 1016: "group", 1020: "url", 1081: "changelog name",
	1082: "changelog text",
}

func (c *textCheck) entry(tag int32, typ, off, count uint32) error {
	switch typ {
	case typeString:
		count = 1
	case typeStringArray, typeI18NString:
	default:
		return nil
	}
	name := textTagNames[tag]
	if name == "" {
		name = fmt.Sprintf("tag %d", tag)
	}
	if uint64(off) >= uint64(len(c.data)) || uint64(count) > uint64(len(c.data))-uint64(off) {
		return fmt.Errorf("%s: offset or count out of range", name)
	}
	rest := c.data[off:]
	for range count {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return fmt.Errorf("%s: unterminated string", name)
		}
		for _, b := range rest[:end] {
			if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
				return fmt.Errorf("%s: control character 0x%02x is not allowed in rpm metadata", name, b)
			}
		}
		rest = rest[end+1:]
		c.scanned += end + 1
	}
	// rpm writes each entry's data once, though real headers can be
	// almost all text (libperf in EL 10: 99.96%). Entries that share data
	// could make this scan quadratic, so text beyond twice the data is
	// refused.
	if c.scanned > 2*len(c.data) {
		return errors.New("header strings overlap")
	}
	return nil
}

type tagValues map[int32]any

func (t tagValues) str(tag int32) string {
	s, _ := t[tag].(string)
	return s
}

func (t tagValues) strs(tag int32) []string {
	s, _ := t[tag].([]string)
	return s
}

func (t tagValues) int(tag int32) (int, bool) {
	v, ok := t[tag].(int32)
	return int(v), ok
}

// readHeader reads one header structure and returns its length in bytes.
// With decode set, it returns the string and int32 values of the tags
// garner reads.
func readHeader(r io.Reader, decode bool) (int, tagValues, error) {
	intro := make([]byte, 16)
	if _, err := io.ReadFull(r, intro); err != nil {
		return 0, nil, errors.New("truncated rpm")
	}
	if !bytes.Equal(intro[:4], headerMagic) {
		return 0, nil, errors.New("bad header magic")
	}
	nindex := binary.BigEndian.Uint32(intro[8:12])
	hsize := binary.BigEndian.Uint32(intro[12:16])
	if uint64(nindex)*16+uint64(hsize) > maxHeader {
		return 0, nil, fmt.Errorf("header too large (%d entries, %d bytes)", nindex, hsize)
	}
	body := make([]byte, int(nindex)*16+int(hsize))
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, errors.New("truncated rpm")
	}
	total := 16 + len(body)
	if !decode {
		return total, nil, nil
	}
	data := body[nindex*16:]
	tags := tagValues{}
	shapes := map[int32][2]uint32{} // type and count
	text := textCheck{data: data}
	for i := uint32(0); i < nindex; i++ {
		e := body[i*16 : i*16+16]
		tag := int32(binary.BigEndian.Uint32(e[0:4]))
		typ := binary.BigEndian.Uint32(e[4:8])
		off := binary.BigEndian.Uint32(e[8:12])
		count := binary.BigEndian.Uint32(e[12:16])
		if err := text.entry(tag, typ, off, count); err != nil {
			return 0, nil, err
		}
		switch tag {
		case tagName, tagVersion, tagRelease, tagEpoch, tagSummary, tagArch, tagSourceRPM, tagObsoletes:
		default:
			continue
		}
		if uint64(off) >= uint64(len(data)) {
			return 0, nil, fmt.Errorf("tag %d: offset out of range", tag)
		}
		var v any
		switch {
		case tag == tagObsoletes && typ == typeStringArray:
			// count strings, each NUL-terminated; each takes a byte at least.
			if uint64(count) > uint64(len(data)) {
				return 0, nil, fmt.Errorf("tag %d: bad count", tag)
			}
			if count > maxObsoletes {
				return 0, nil, fmt.Errorf("%d obsoletes, limit %d", count, maxObsoletes)
			}
			rest, names := data[off:], make([]string, 0, count)
			for range count {
				end := bytes.IndexByte(rest, 0)
				if end < 0 {
					return 0, nil, fmt.Errorf("tag %d: unterminated string", tag)
				}
				names = append(names, string(rest[:end]))
				rest = rest[end+1:]
			}
			v = names
		case tag == tagObsoletes:
			return 0, nil, fmt.Errorf("tag %d: type %d, want a string array", tag, typ)
		case tag == tagEpoch:
			if typ != typeInt32 || count != 1 {
				return 0, nil, fmt.Errorf("tag %d: type %d count %d, want one int32", tag, typ, count)
			}
			if uint64(off)+4 > uint64(len(data)) {
				return 0, nil, fmt.Errorf("tag %d: bad int32", tag)
			}
			v = int32(binary.BigEndian.Uint32(data[off : off+4]))
		// Only the summary may be an I18N string: rpm picks one of its
		// values by locale, and garner reads the first.
		case typ == typeString, tag == tagSummary && typ == typeI18NString:
			end := bytes.IndexByte(data[off:], 0)
			if end < 0 {
				return 0, nil, fmt.Errorf("tag %d: unterminated string", tag)
			}
			v = string(data[off : off+uint32(end)])
		default:
			return 0, nil, fmt.Errorf("tag %d: type %d, want a string", tag, typ)
		}
		// rpm finds a tag by binary search, so of two copies it may read
		// another one than garner. Identical copies cannot disagree; some
		// AlmaLinux 10 builds repeat SOURCERPM this way.
		if prev, dup := tags[tag]; dup {
			if shapes[tag] != [2]uint32{typ, count} || !reflect.DeepEqual(prev, v) {
				return 0, nil, fmt.Errorf("tag %d: duplicate entry with a different value", tag)
			}
			continue
		}
		tags[tag], shapes[tag] = v, [2]uint32{typ, count}
	}
	return total, tags, nil
}
