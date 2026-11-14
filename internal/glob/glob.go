// Package glob checks whether two glob patterns can match the same name.
//
// Patterns use path.Match syntax: '*' matches any run of characters, '?'
// matches one character, '[...]' matches a character class ('[^...]'
// negates it), and '\' escapes the next character. Names are package and
// repository names, which never contain '/', so '*' is treated as matching
// any character.
package glob

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

// Validate reports whether pattern is a well-formed glob.
func Validate(pattern string) error {
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	_, err := parse(pattern)
	return err
}

// Overlap reports whether some name matches both a and b. When it does, it
// also returns one such name, for use in error messages.
func Overlap(a, b string) (example string, ok bool, err error) {
	pa, err := parse(a)
	if err != nil {
		return "", false, err
	}
	pb, err := parse(b)
	if err != nil {
		return "", false, err
	}
	s := solver{p: pa, q: pb, memo: map[[2]int]bool{}}
	if !s.overlap(0, 0) {
		return "", false, nil
	}
	return s.example(), true, nil
}

// token is one element of a parsed pattern: a star, or a single-character
// set (a literal, '?', or a class).
type token struct {
	star bool
	set  charSet
}

// charSet is a union of rune ranges, optionally negated.
type charSet struct {
	negated bool
	ranges  [][2]rune
}

func (c charSet) contains(r rune) bool {
	in := false
	for _, rg := range c.ranges {
		if rg[0] <= r && r <= rg[1] {
			in = true
			break
		}
	}
	return in != c.negated
}

var anyChar = charSet{negated: true}

func parse(pattern string) ([]token, error) {
	var toks []token
	for i := 0; i < len(pattern); {
		switch c := pattern[i]; c {
		case '*':
			// Consecutive stars are equivalent to one.
			if len(toks) == 0 || !toks[len(toks)-1].star {
				toks = append(toks, token{star: true})
			}
			i++
		case '?':
			toks = append(toks, token{set: anyChar})
			i++
		case '[':
			set, n, err := parseClass(pattern[i+1:])
			if err != nil {
				return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
			}
			toks = append(toks, token{set: set})
			i += 1 + n
		default:
			if c == '\\' {
				i++
				if i == len(pattern) {
					return nil, fmt.Errorf("invalid glob %q: trailing backslash", pattern)
				}
			}
			r, size := utf8.DecodeRuneInString(pattern[i:])
			toks = append(toks, token{set: charSet{ranges: [][2]rune{{r, r}}}})
			i += size
		}
	}
	return toks, nil
}

// parseClass parses the body of a character class, after '['. It returns the
// set and the number of bytes consumed, including the closing ']'.
func parseClass(s string) (charSet, int, error) {
	var set charSet
	i := 0
	if i < len(s) && s[i] == '^' {
		set.negated = true
		i++
	}
	readRune := func() (rune, error) {
		if i < len(s) && s[i] == '\\' {
			i++
		}
		if i >= len(s) {
			return 0, errors.New("unterminated character class")
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		return r, nil
	}
	for first := true; ; first = false {
		if i >= len(s) {
			return charSet{}, 0, errors.New("unterminated character class")
		}
		if s[i] == ']' && !first {
			return set, i + 1, nil
		}
		lo, err := readRune()
		if err != nil {
			return charSet{}, 0, err
		}
		hi := lo
		if i+1 < len(s) && s[i] == '-' && s[i+1] != ']' {
			i++
			if hi, err = readRune(); err != nil {
				return charSet{}, 0, err
			}
			if hi < lo {
				return charSet{}, 0, fmt.Errorf("bad range %q-%q", lo, hi)
			}
		}
		set.ranges = append(set.ranges, [2]rune{lo, hi})
	}
}

// solver decides whether two token sequences match a common string. State
// (i, j) means: can p[i:] and q[j:] match the same remaining string? The
// state space is len(p)*len(q), so the check is quadratic at worst.
type solver struct {
	p, q []token
	memo map[[2]int]bool
}

func (s *solver) overlap(i, j int) bool {
	key := [2]int{i, j}
	if v, ok := s.memo[key]; ok {
		return v
	}
	v := s.step(i, j) // every step increases i+j, so recursion terminates
	s.memo[key] = v
	return v
}

func (s *solver) step(i, j int) bool {
	pEnd, qEnd := i == len(s.p), j == len(s.q)
	switch {
	case pEnd && qEnd:
		return true
	case !pEnd && s.p[i].star:
		// The star matches nothing, or absorbs the next character of q.
		return s.overlap(i+1, j) || (!qEnd && s.overlap(i, j+1))
	case !qEnd && s.q[j].star:
		return s.overlap(i, j+1) || (!pEnd && s.overlap(i+1, j))
	case pEnd || qEnd:
		return false
	default:
		_, ok := commonRune(s.p[i].set, s.q[j].set)
		return ok && s.overlap(i+1, j+1)
	}
}

// example rebuilds one common string by replaying the choices that made
// overlap(0, 0) true.
func (s *solver) example() string {
	var b strings.Builder
	i, j := 0, 0
	for i < len(s.p) || j < len(s.q) {
		pEnd, qEnd := i == len(s.p), j == len(s.q)
		switch {
		case !pEnd && s.p[i].star:
			if s.overlap(i+1, j) {
				i++
				continue
			}
			if !s.q[j].star {
				r, _ := commonRune(anyChar, s.q[j].set)
				b.WriteRune(r)
			}
			j++
		case !qEnd && s.q[j].star:
			if s.overlap(i, j+1) {
				j++
				continue
			}
			r, _ := commonRune(s.p[i].set, anyChar)
			b.WriteRune(r)
			i++
		default:
			r, _ := commonRune(s.p[i].set, s.q[j].set)
			b.WriteRune(r)
			i++
			j++
		}
	}
	return b.String()
}

// commonRune returns a rune contained in both sets. Two unions of ranges
// intersect exactly when one of the boundary runes below lies in both, so
// testing those candidates is complete. Readable runes are tried first so
// examples look like package names.
func commonRune(a, b charSet) (rune, bool) {
	const preferred = "abcdefghijklmnopqrstuvwxyz0123456789-._+"
	for _, r := range preferred {
		if a.contains(r) && b.contains(r) {
			return r, true
		}
	}
	var cands []rune
	for _, set := range []charSet{a, b} {
		for _, rg := range set.ranges {
			cands = append(cands, rg[0], rg[1], rg[0]-1, rg[1]+1)
		}
	}
	cands = append(cands, 1, utf8.MaxRune)
	sort.Slice(cands, func(x, y int) bool { return cands[x] < cands[y] })
	for _, r := range cands {
		if r > 0 && r <= utf8.MaxRune && a.contains(r) && b.contains(r) {
			return r, true
		}
	}
	return 0, false
}
