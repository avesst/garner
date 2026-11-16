package deb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// The xz decoder allocates the LZMA2 dictionary a block header declares,
// up to 4 GiB, before any limit on its output applies. checkXZ rejects a
// stream that declares more than maxXZDict, which is what xz -9 uses. It
// needs the whole compressed member in memory, hence maxXZControl.
const (
	maxXZDict    = 64 << 20
	maxXZControl = 64 << 20
)

var xzMagic = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}

// xzCheckSizes maps the check type in the stream flags to its size.
var xzCheckSizes = [16]int{0, 4, 4, 4, 8, 8, 8, 16, 16, 16, 32, 32, 32, 64, 64, 64}

// checkXZ walks every block header of the xz data in b, in the order the
// decoder reads them, and fails if one declares a dictionary larger than
// maxXZDict or a filter the decoder does not support. Blocks are located by
// following the sizes in the LZMA2 chunk headers, not through the index,
// which the decoder only checks after it has decoded the blocks.
func checkXZ(b []byte) error {
	x := &xzScanner{b: b}
	for {
		if err := x.stream(); err != nil {
			return err
		}
		// Stream padding, then another stream or the end.
		for len(x.b)-x.i >= 4 && bytes.Equal(x.b[x.i:x.i+4], []byte{0, 0, 0, 0}) {
			x.i += 4
		}
		if x.i == len(x.b) {
			return nil
		}
	}
}

type xzScanner struct {
	b []byte
	i int
}

var errXZTruncated = errors.New("truncated xz data")

func (x *xzScanner) take(n int) ([]byte, error) {
	if n < 0 || n > len(x.b)-x.i {
		return nil, errXZTruncated
	}
	s := x.b[x.i : x.i+n]
	x.i += n
	return s, nil
}

func (x *xzScanner) uvarint() (uint64, error) {
	v, n := binary.Uvarint(x.b[x.i:])
	if n <= 0 {
		return 0, errors.New("bad xz integer")
	}
	x.i += n
	return v, nil
}

// stream reads one stream: header, blocks, index and footer.
func (x *xzScanner) stream() error {
	h, err := x.take(12)
	if err != nil {
		return err
	}
	if !bytes.Equal(h[:6], xzMagic) {
		return errors.New("bad xz stream header")
	}
	if h[6] != 0 || h[7] > 0x0f {
		return errors.New("unsupported xz stream flags")
	}
	checkSize := xzCheckSizes[h[7]]
	for {
		start := x.i
		sb, err := x.take(1)
		if err != nil {
			return err
		}
		if sb[0] == 0 {
			break // index indicator
		}
		if err := x.block(start, (int(sb[0])+1)*4, checkSize); err != nil {
			return err
		}
	}

	// The index: record count, records, padding, CRC32; then the footer.
	indexStart := x.i - 1
	n, err := x.uvarint()
	if err != nil {
		return err
	}
	for ; n > 0; n-- {
		if _, err := x.uvarint(); err != nil {
			return err
		}
		if _, err := x.uvarint(); err != nil {
			return err
		}
	}
	if _, err := x.take((4-(x.i-indexStart)%4)%4 + 4); err != nil {
		return err
	}
	f, err := x.take(12)
	if err != nil {
		return err
	}
	if string(f[10:]) != "YZ" {
		return errors.New("bad xz stream footer")
	}
	return nil
}

// block reads one block whose header of size bytes starts at start, after
// its first byte has been read.
func (x *xzScanner) block(start, size, checkSize int) error {
	h, err := x.take(size - 1)
	if err != nil {
		return err
	}
	if err := checkXZBlockHeader(h[:len(h)-4]); err != nil {
		return err
	}
	for {
		c, err := x.take(1)
		if err != nil {
			return err
		}
		switch ctl := c[0]; {
		case ctl == 0x00: // end of the LZMA2 data
			_, err := x.take((4-(x.i-start)%4)%4 + checkSize)
			return err
		case ctl == 0x01 || ctl == 0x02: // uncompressed chunk
			s, err := x.take(2)
			if err != nil {
				return err
			}
			if _, err := x.take(int(binary.BigEndian.Uint16(s)) + 1); err != nil {
				return err
			}
		case ctl >= 0x80: // LZMA chunk, with a properties byte from 0xc0
			n := 4
			if ctl >= 0xc0 {
				n = 5
			}
			s, err := x.take(n)
			if err != nil {
				return err
			}
			if _, err := x.take(int(binary.BigEndian.Uint16(s[2:4])) + 1); err != nil {
				return err
			}
		default:
			return fmt.Errorf("bad LZMA2 chunk control byte %#x", ctl)
		}
	}
}

// checkXZBlockHeader checks a block header without its size byte and CRC32.
func checkXZBlockHeader(h []byte) error {
	if len(h) == 0 {
		return errXZTruncated
	}
	flags := h[0]
	if flags&0x3c != 0 {
		return errors.New("reserved xz block flags set")
	}
	if flags&0x03 != 0 {
		return errors.New("unsupported xz filter chain")
	}
	r := &xzScanner{b: h, i: 1}
	for _, present := range []bool{flags&0x40 != 0, flags&0x80 != 0} {
		if present { // compressed and uncompressed size
			if _, err := r.uvarint(); err != nil {
				return err
			}
		}
	}
	id, err := r.uvarint()
	if err != nil {
		return err
	}
	if id != 0x21 {
		return fmt.Errorf("unsupported xz filter %#x", id)
	}
	if n, err := r.uvarint(); err != nil || n != 1 {
		return errors.New("bad LZMA2 filter properties")
	}
	p, err := r.take(1)
	if err != nil {
		return err
	}
	if d := xzDictSize(p[0]); d > maxXZDict {
		return fmt.Errorf("LZMA2 dictionary of %d bytes exceeds the limit of %d", d, maxXZDict)
	}
	return nil
}

// xzDictSize decodes the LZMA2 dictionary size property. Values above 40
// are invalid and treated as unlimited.
func xzDictSize(p byte) uint64 {
	switch {
	case p > 40:
		return 1 << 63
	case p == 40:
		return 1<<32 - 1
	}
	return uint64(2|p&1) << (p/2 + 11)
}
