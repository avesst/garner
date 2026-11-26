package deb

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// relationCases are control fields and whether dpkg accepts them. Each
// rejected case either breaks apt-get update for every client of the
// suite (marked apt) or makes dpkg refuse to install the package.
var relationCases = []struct {
	name  string
	field string
	ok    bool
}{
	{"empty", "Depends:", true},
	{"one", "Depends: libc6", true},
	{"operators", "Depends: a1 (<< 1), a2 (<= 1), a3 (= 1), a4 (>= 1), a5 (>> 1)", true},
	{"obsolete operators", "Depends: a1 (< 1), a2 (> 1)", true},
	{"implicit exact", "Depends: libc6 (2.36)", true},
	{"spacing", "Depends: liba ( >= 1.0 ) , libb(>=2),libc", true},
	{"folded", "Depends: liba (>= 1),\n libb,\n\tlibc", true},
	{"epoch and tilde", "Depends: liba (>= 1:2.0-1~rc1+b1)", true},
	{"arch qualifiers", "Depends: a1:any, a2:native, a3:amd64, a4:any (>= 1)", true},
	{"alternatives", "Depends: liba | libb (>= 2) | libc", true},
	{"plus in name", "Depends: libstdc++6 (>= 13)", true},
	{"upper case", "Depends: LibA", true},
	{"one character", "Depends: a", true},
	{"underscore", "Depends: lib_a", true},
	{"duplicate", "Depends: liba, liba", true},
	{"pre-depends alternatives", "Pre-Depends: liba | libb", true},
	{"recommends alternatives", "Recommends: liba | libb", true},
	{"suggests alternatives", "Suggests: liba | libb", true},
	{"enhances alternatives", "Enhances: liba | libb", true},
	{"provides exact", "Provides: liba (= 1)", true},
	{"provides qualifier", "Provides: liba:any, libb:amd64", true},
	{"breaks version", "Breaks: liba (<< 2)", true},
	{"replaces qualifier", "Replaces: liba:any", true},
	{"conflicts qualifier", "Conflicts: liba:amd64", true},
	{"lower-case field name", "depends: liba", true},

	{"unclosed (apt)", "Depends: libc6 (>= 2.31", false},
	{"arch list (apt)", "Depends: libc6 [amd64]", false},
	{"replaces arch list (apt)", "Replaces: other [amd64]", false},
	{"build profile (apt)", "Depends: liba <!nocheck>", false},
	{"missing comma (apt)", "Depends: libc6 libfoo", false},
	{"empty version (apt)", "Depends: libc6 ()", false},
	{"stray paren (apt)", "Depends: libc6)", false},
	{"leading paren (apt)", "Breaks: (foo)", false},
	{"provides unclosed (apt)", "Provides: foo (", false},
	{"leading comma (apt)", "Depends: , liba", false},
	{"double comma (apt)", "Depends: liba,, libb", false},
	{"trailing comma", "Depends: liba,", false},
	{"trailing bar", "Depends: liba |", false},
	{"empty qualifier", "Depends: liba:", false},
	{"two versions", "Depends: liba (>= 1 2)", false},
	{"version without digit", "Depends: liba (>= a1)", false},
	{"bad name character", "Depends: li$ba", false},
	{"name starts with dash", "Depends: -liba", false},
	{"space before qualifier", "Depends: liba :any", false},
	{"conflicts alternatives", "Conflicts: liba | libb", false},
	{"breaks alternatives", "Breaks: liba | libb", false},
	{"replaces alternatives", "Replaces: liba | libb", false},
	{"provides alternatives", "Provides: liba | libb", false},
	{"provides not exact", "Provides: liba (>= 1)", false},
}

func TestRelationFields(t *testing.T) {
	for _, tc := range relationCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Read(bytes.NewReader(debtestControl(t, tc.field)))
			if tc.ok && err != nil {
				t.Errorf("%q rejected: %v", tc.field, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("%q accepted", tc.field)
			}
		})
	}
}

// strictInNewerDpkg are cases garner rejects that dpkg before 1.22.22
// (Debian 12, Ubuntu 22.04 and 24.04) builds and installs with a warning,
// and that Debian 13's and later dpkg refuse to build or install.
var strictInNewerDpkg = map[string]bool{"provides not exact": true}

// TestRelationFieldsMatchDpkg builds each case with dpkg-deb, which
// parses relationship fields with dpkg's own parser, and checks that
// garner accepts exactly what dpkg accepts.
func TestRelationFieldsMatchDpkg(t *testing.T) {
	dpkgDeb, err := exec.LookPath("dpkg-deb")
	if err != nil {
		t.Skip("dpkg-deb not installed")
	}
	for _, tc := range relationCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "pkg", "DEBIAN"), 0o755); err != nil {
				t.Fatal(err)
			}
			// Not last: dpkg rejects an empty value on the last line of a file.
			control := "Package: tool\nVersion: 1.0-1\nArchitecture: amd64\n" + tc.field + "\nMaintainer: x <x@example.com>\nDescription: x\n"
			if err := os.WriteFile(filepath.Join(dir, "pkg", "DEBIAN", "control"), []byte(control), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(dpkgDeb, "--build", "--root-owner-group", filepath.Join(dir, "pkg"), filepath.Join(dir, "p.deb")).CombinedOutput()
			var exit *exec.ExitError
			if err != nil && !errors.As(err, &exit) {
				t.Fatal(err)
			}
			// Not a skip: CI fails on skipped tool tests, which mean a tool
			// is missing.
			if err == nil && !tc.ok && strictInNewerDpkg[tc.name] {
				t.Logf("this dpkg-deb only warns about %q; dpkg 1.22.22 (Debian 13) and later refuse it\n%s", tc.field, out)
				return
			}
			if dpkgOK := err == nil; dpkgOK != tc.ok {
				t.Errorf("dpkg-deb accepts %q: %v, the case expects %v\n%s", tc.field, dpkgOK, tc.ok, out)
			}
		})
	}
}

func TestParseRelations(t *testing.T) {
	rels, err := parseRelations("liba:any (>= 1.2) | libb, libc ( << 2 )", true, false)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]relation{
		{{Name: "liba", Arch: "any", Op: ">=", Version: "1.2"}, {Name: "libb"}},
		{{Name: "libc", Op: "<<", Version: "2"}},
	}
	if len(rels) != len(want) {
		t.Fatalf("got %v, want %v", rels, want)
	}
	for i := range want {
		if strings.Join(relStrings(rels[i]), "|") != strings.Join(relStrings(want[i]), "|") {
			t.Errorf("entry %d = %v, want %v", i, rels[i], want[i])
		}
	}
}

func relStrings(alts []relation) []string {
	var out []string
	for _, r := range alts {
		out = append(out, r.Name+":"+r.Arch+" "+r.Op+" "+r.Version)
	}
	return out
}

// debtestControl builds a deb whose control file carries field in
// addition to the required ones.
func debtestControl(t *testing.T, field string) []byte {
	t.Helper()
	control := strings.Replace(testControl, "Installed-Size: 12\n", "Installed-Size: 12\n"+field+"\n", 1)
	return buildDeb(t, debMember{"control.tar", controlTar(t, "./control", control)})
}
