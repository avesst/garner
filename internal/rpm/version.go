// Package rpm parses rpm package headers, compares versions like rpm, and
// publishes rpm repositories with createrepo_c (design sections 4, 9, 10).
package rpm

// Vercmp is rpm's rpmvercmp: it compares two version or release strings
// and returns -1, 0 or 1. Alphanumeric segments are compared, numeric ones
// as numbers; '~' sorts before everything, even the end of the string,
// and '^' sorts after the end but before anything else.
func Vercmp(a, b string) int {
	if a == b {
		return 0
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for i < len(a) && !isAlnum(a[i]) && a[i] != '~' && a[i] != '^' {
			i++
		}
		for j < len(b) && !isAlnum(b[j]) && b[j] != '~' && b[j] != '^' {
			j++
		}
		ca, cb := at(a, i), at(b, j)
		if ca == '~' || cb == '~' {
			if ca != '~' {
				return 1
			}
			if cb != '~' {
				return -1
			}
			i++
			j++
			continue
		}
		if ca == '^' || cb == '^' {
			if i >= len(a) {
				return -1
			}
			if j >= len(b) {
				return 1
			}
			if ca != '^' {
				return 1
			}
			if cb != '^' {
				return -1
			}
			i++
			j++
			continue
		}
		if i >= len(a) || j >= len(b) {
			break
		}
		si, sj := i, j
		numeric := isDigit(a[i])
		if numeric {
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
		} else {
			for i < len(a) && isAlpha(a[i]) {
				i++
			}
			for j < len(b) && isAlpha(b[j]) {
				j++
			}
		}
		segA, segB := a[si:i], b[sj:j]
		if segB == "" {
			// Segments of different types: numeric is newer.
			if numeric {
				return 1
			}
			return -1
		}
		if numeric {
			segA, segB = trimZeros(segA), trimZeros(segB)
			if len(segA) != len(segB) {
				if len(segA) > len(segB) {
					return 1
				}
				return -1
			}
		}
		if segA != segB {
			if segA < segB {
				return -1
			}
			return 1
		}
	}
	switch {
	case i >= len(a) && j >= len(b):
		return 0
	case i < len(a):
		return 1
	default:
		return -1
	}
}

// CompareEVR compares epoch, version and release in that order.
func CompareEVR(e1 int, v1, r1 string, e2 int, v2, r2 string) int {
	switch {
	case e1 < e2:
		return -1
	case e1 > e2:
		return 1
	}
	if c := Vercmp(v1, v2); c != 0 {
		return c
	}
	return Vercmp(r1, r2)
}

func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func trimZeros(s string) string {
	for len(s) > 0 && s[0] == '0' {
		s = s[1:]
	}
	return s
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }
