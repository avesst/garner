package glob

import (
	"path"
	"testing"
)

func TestOverlap(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"tool", "tool", true},
		{"tool", "tools", false},
		{"tool-*", "tool-cli", true},
		{"tool-*", "*-cli", true},
		{"tool-*", "lib*", false},
		{"*", "anything", true},
		{"*", "", true},
		{"a*b", "*c", false},
		{"a*b", "a*", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"[a-c]x", "bx", true},
		{"[a-c]x", "dx", false},
		{"[^a-c]x", "[a-c]x", false},
		{"[^a-c]x", "[a-d]x", true},
		{"lib*-dev", "*-dbg", false},
		{"lib*-dev", "libfoo*", true},
		{`a\*`, "a*", true},
		{`a\*`, "ab", false},
		{"**x", "x", true},
		{"*a*", "*b*", true},
	}
	for _, tt := range tests {
		for _, pair := range [][2]string{{tt.a, tt.b}, {tt.b, tt.a}} {
			ex, ok, err := Overlap(pair[0], pair[1])
			if err != nil {
				t.Fatalf("Overlap(%q, %q): %v", pair[0], pair[1], err)
			}
			if ok != tt.want {
				t.Errorf("Overlap(%q, %q) = %v, want %v", pair[0], pair[1], ok, tt.want)
				continue
			}
			if !ok {
				continue
			}
			// The example must really match both patterns.
			for _, p := range pair {
				if m, _ := path.Match(p, ex); !m {
					t.Errorf("Overlap(%q, %q) example %q does not match %q", pair[0], pair[1], ex, p)
				}
			}
		}
	}
}

// TestOverlapBruteForce compares Overlap with path.Match over every pattern
// pair up to length 3 from a small alphabet. If two patterns share a match,
// one of length at most len(a)+len(b) exists over {a, b, c}, so enumerating
// those strings decides the answer independently.
func TestOverlapBruteForce(t *testing.T) {
	patterns := allStrings("ab*?", 3)
	names := allStrings("abc", 6)
	for _, a := range patterns {
		for _, b := range patterns {
			want := false
			for _, n := range names {
				ma, _ := path.Match(a, n)
				mb, _ := path.Match(b, n)
				if ma && mb {
					want = true
					break
				}
			}
			if _, got, _ := Overlap(a, b); got != want {
				t.Errorf("Overlap(%q, %q) = %v, brute force says %v", a, b, got, want)
			}
		}
	}
}

func allStrings(alphabet string, maxLen int) []string {
	out := []string{""}
	level := []string{""}
	for n := 0; n < maxLen; n++ {
		var next []string
		for _, s := range level {
			for _, c := range alphabet {
				next = append(next, s+string(c))
			}
		}
		out = append(out, next...)
		level = next
	}
	return out
}

func TestOverlapExampleIsReadable(t *testing.T) {
	ex, ok, err := Overlap("tool-*", "*-cli")
	if err != nil || !ok {
		t.Fatalf("Overlap = %q, %v, %v", ex, ok, err)
	}
	if ex != "tool-cli" {
		t.Errorf("example = %q, want %q", ex, "tool-cli")
	}
}

func TestValidate(t *testing.T) {
	for _, p := range []string{"tool", "tool-*", "[a-z]*", `a\*`} {
		if err := Validate(p); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"[a-", `a\`, "[z-a]"} {
		if err := Validate(p); err == nil {
			t.Errorf("Validate(%q) = nil, want error", p)
		}
	}
}

// TestMatchAgreesWithPathMatch checks Match against path.Match on names
// without '/', where the two must agree.
func TestMatchAgreesWithPathMatch(t *testing.T) {
	patterns := append(allStrings("ab*?", 3), "[a-b]*", "[^a]?", `a\*`, "*b*")
	for _, p := range patterns {
		for _, n := range allStrings("abc", 4) {
			want, _ := path.Match(p, n)
			if got := Match(p, n); got != want {
				t.Errorf("Match(%q, %q) = %v, path.Match says %v", p, n, got, want)
			}
		}
	}
}

func TestMatchSlash(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"team/*", "team/tool", true},
		{"team/*", "team/tools/cli", true},
		{"team/*", "team", false},
		{"team/*", "other/tool", false},
		{"*", "a/b", true},
		{"a?b", "a/b", true},
		{"tool", "tool/x", false},
		{"[", "[", false}, // invalid pattern
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.name); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

// TestOverlapAgreesWithMatch brute-forces Overlap against Match over an
// alphabet with '/', so validation and ingest decide ownership alike.
func TestOverlapAgreesWithMatch(t *testing.T) {
	patterns := allStrings("a/*?", 3)
	names := allStrings("a/b", 6)
	for _, a := range patterns {
		for _, b := range patterns {
			want := false
			for _, n := range names {
				if Match(a, n) && Match(b, n) {
					want = true
					break
				}
			}
			ex, got, _ := Overlap(a, b)
			if got != want {
				t.Errorf("Overlap(%q, %q) = %v, brute force says %v", a, b, got, want)
			}
			if got && (!Match(a, ex) || !Match(b, ex)) {
				t.Errorf("Overlap(%q, %q) example %q does not match both", a, b, ex)
			}
		}
	}
}
