// Package deb parses Debian binary packages and compares their versions the
// way dpkg does (design sections 6 and 7).
package deb

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a Debian version: [epoch:]upstream[-revision].
type Version struct {
	Epoch    uint32
	Upstream string
	Revision string
}

// ParseVersion parses s with dpkg's rules. Unlike dpkg, which only warns,
// it rejects an upstream version that does not start with a digit, because
// garner should not accept a package that dpkg-deb would complain about.
func ParseVersion(s string) (Version, error) {
	var v Version
	if s == "" {
		return v, errors.New("version is empty")
	}
	if strings.ContainsFunc(s, isSpace) {
		return v, fmt.Errorf("version %q contains whitespace", s)
	}
	rest := s
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		if i == 0 {
			return v, fmt.Errorf("version %q: epoch is empty", s)
		}
		e, err := strconv.ParseUint(rest[:i], 10, 32)
		if err != nil || !allDigits(rest[:i]) {
			return v, fmt.Errorf("version %q: epoch is not a number", s)
		}
		v.Epoch = uint32(e)
		rest = rest[i+1:]
		if rest == "" {
			return v, fmt.Errorf("version %q: nothing after the epoch", s)
		}
	}
	if i := strings.LastIndexByte(rest, '-'); i >= 0 {
		v.Revision = rest[i+1:]
		rest = rest[:i]
		if v.Revision == "" {
			return v, fmt.Errorf("version %q: revision is empty", s)
		}
	}
	v.Upstream = rest
	if v.Upstream == "" {
		return v, fmt.Errorf("version %q: upstream version is empty", s)
	}
	if !isDigit(v.Upstream[0]) {
		return v, fmt.Errorf("version %q: upstream version must start with a digit", s)
	}
	for _, c := range []byte(v.Upstream) {
		if !isAlnum(c) && !strings.ContainsRune(".-+~:", rune(c)) {
			return v, fmt.Errorf("version %q: invalid character %q in upstream version", s, c)
		}
	}
	for _, c := range []byte(v.Revision) {
		if !isAlnum(c) && !strings.ContainsRune(".+~", rune(c)) {
			return v, fmt.Errorf("version %q: invalid character %q in revision", s, c)
		}
	}
	return v, nil
}

// String formats v the way dpkg prints it: the epoch only when non-zero,
// the revision only when present.
func (v Version) String() string {
	var b strings.Builder
	if v.Epoch != 0 {
		b.WriteString(strconv.FormatUint(uint64(v.Epoch), 10))
		b.WriteByte(':')
	}
	b.WriteString(v.Upstream)
	if v.Revision != "" {
		b.WriteByte('-')
		b.WriteString(v.Revision)
	}
	return b.String()
}

// Compare returns -1, 0 or +1 as a is older than, equal to or newer than b.
// A missing revision compares like "0", as in dpkg.
func Compare(a, b Version) int {
	switch {
	case a.Epoch < b.Epoch:
		return -1
	case a.Epoch > b.Epoch:
		return 1
	}
	if c := compareString(a.Upstream, b.Upstream); c != 0 {
		return c
	}
	return compareString(a.Revision, b.Revision)
}

// compareString is dpkg's verrevcmp. It alternates between a non-digit run,
// compared character by character with order(), and a digit run, compared
// numerically.
func compareString(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			ac, bc := 0, 0
			if i < len(a) {
				ac = order(a[i])
			}
			if j < len(b) {
				bc = order(b[j])
			}
			if ac != bc {
				return sign(ac - bc)
			}
			i++
			j++
		}
		for i < len(a) && a[i] == '0' {
			i++
		}
		for j < len(b) && b[j] == '0' {
			j++
		}
		first := 0
		for i < len(a) && isDigit(a[i]) && j < len(b) && isDigit(b[j]) {
			if first == 0 {
				first = int(a[i]) - int(b[j])
			}
			i++
			j++
		}
		if i < len(a) && isDigit(a[i]) {
			return 1
		}
		if j < len(b) && isDigit(b[j]) {
			return -1
		}
		if first != 0 {
			return sign(first)
		}
	}
	return 0
}

// order ranks a non-digit character: '~' sorts before everything, even the
// end of the string; letters sort before other characters.
func order(c byte) int {
	switch {
	case isDigit(c):
		return 0
	case isAlpha(c):
		return int(c)
	case c == '~':
		return -1
	default:
		return int(c) + 256
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }
func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

func allDigits(s string) bool {
	for _, c := range []byte(s) {
		if !isDigit(c) {
			return false
		}
	}
	return true
}
