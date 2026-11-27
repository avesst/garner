package glob

import (
	"testing"
	"unicode/utf8"
)

// FuzzOverlap checks Overlap against Match: a name both globs match means
// they overlap, and an example Overlap gives must match both.
func FuzzOverlap(f *testing.F) {
	f.Add("tool-*", "*-cli", "tool-cli")
	f.Add("[a-c]?x", "b[^q]*", "bax")
	f.Add(`\*a`, "*", "*a")
	f.Add("[]a]", "]", "]")
	f.Add("team/*", "*/tool", "team/tool")
	f.Fuzz(func(t *testing.T, a, b, name string) {
		if !utf8.ValidString(a+b+name) || Validate(a) != nil || Validate(b) != nil {
			return
		}
		example, ok, err := Overlap(a, b)
		if err != nil {
			t.Fatalf("Overlap(%q, %q): %v, though both validate", a, b, err)
		}
		if Match(a, name) && Match(b, name) && !ok {
			t.Fatalf("%q matches %q and %q, but Overlap reports none", name, a, b)
		}
		if ok && (!Match(a, example) || !Match(b, example)) {
			t.Fatalf("Overlap(%q, %q) example %q does not match both", a, b, example)
		}
	})
}
