package rpm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

// Package is what garner reads from an rpm header.
type Package struct {
	Name      string
	Epoch     int
	Version   string
	Release   string
	Arch      string
	Summary   string
	SourceRPM string
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
	switch {
	case p.Name == "" || p.Version == "" || p.Release == "" || p.Arch == "":
		return nil, errors.New("rpm header lacks name, version, release or arch")
	case p.SourceRPM == "":
		return nil, errors.New("source rpms are not accepted")
	case p.Epoch < 0:
		return nil, errors.New("negative epoch")
	}
	return p, nil
}

type tagValues map[int32]any

func (t tagValues) str(tag int32) string {
	s, _ := t[tag].(string)
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
	for i := uint32(0); i < nindex; i++ {
		e := body[i*16 : i*16+16]
		tag := int32(binary.BigEndian.Uint32(e[0:4]))
		typ := binary.BigEndian.Uint32(e[4:8])
		off := binary.BigEndian.Uint32(e[8:12])
		count := binary.BigEndian.Uint32(e[12:16])
		switch tag {
		case tagName, tagVersion, tagRelease, tagEpoch, tagSummary, tagArch, tagSourceRPM:
		default:
			continue
		}
		if uint64(off) >= uint64(len(data)) {
			return 0, nil, fmt.Errorf("tag %d: offset out of range", tag)
		}
		switch typ {
		case typeString, typeI18NString, typeStringArray:
			end := bytes.IndexByte(data[off:], 0)
			if end < 0 {
				return 0, nil, fmt.Errorf("tag %d: unterminated string", tag)
			}
			tags[tag] = string(data[off : off+uint32(end)])
		case typeInt32:
			if count < 1 || uint64(off)+4 > uint64(len(data)) {
				return 0, nil, fmt.Errorf("tag %d: bad int32", tag)
			}
			tags[tag] = int32(binary.BigEndian.Uint32(data[off : off+4]))
		}
	}
	return total, tags, nil
}
