package deb

import (
	"errors"
	"os/exec"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want Version
	}{
		{"1.0", Version{0, "1.0", ""}},
		{"1.0-1", Version{0, "1.0", "1"}},
		{"2:1.0-1", Version{2, "1.0", "1"}},
		{"1.0-rc1-2", Version{0, "1.0-rc1", "2"}},
		{"1:2:3", Version{1, "2:3", ""}},
		{"1.2~beta+dfsg-0ubuntu1~24.04", Version{0, "1.2~beta+dfsg", "0ubuntu1~24.04"}},
		{"0:1", Version{0, "1", ""}},
	}
	for _, tt := range tests {
		got, err := ParseVersion(tt.in)
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseVersionInvalid(t *testing.T) {
	for _, in := range []string{
		"", ":1", "1:", "a:1", "-1:1", "99999999999:1", "1-", "-1",
		"a1", "1 2", "1_2", "1.0-a_b", "1.0-1:2", " 1",
	} {
		if v, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) = %+v, want an error", in, v)
		}
	}
}

func TestVersionString(t *testing.T) {
	for _, in := range []string{"1.0", "1.0-1", "3:1.0-1", "1:2:3"} {
		v, err := ParseVersion(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != in {
			t.Errorf("String() = %q, want %q", got, in)
		}
	}
	if got := mustParse(t, "0:1-2").String(); got != "1-2" {
		t.Errorf("zero epoch: String() = %q, want %q", got, "1-2")
	}
	// Without the epoch, the colon would read as the epoch separator.
	if got := mustParse(t, "0:1:2").String(); got != "0:1:2" {
		t.Errorf("zero epoch, colon in upstream: String() = %q, want %q", got, "0:1:2")
	}
}

// compareTests are written for garner; they cover each rule of dpkg's
// algorithm. TestCompareMatchesDpkg checks the same algorithm against the
// real dpkg.
var compareTests = []struct {
	a, b string
	want int
}{
	{"1.0", "1.0", 0},
	{"1.0", "1.1", -1},
	{"1.10", "1.9", 1},     // numeric, not lexical
	{"1.010", "1.10", 0},   // leading zeros are ignored
	{"1.0", "1.0-0", 0},    // missing revision equals "0"
	{"1:0.1", "2.0", 1},    // epoch wins
	{"0:2.0", "2.0", 0},    // explicit zero epoch
	{"1.0~rc1", "1.0", -1}, // tilde sorts before the end
	{"1.0~~", "1.0~", -1},  // and before another tilde
	{"1.0~rc1", "1.0~rc2", -1},
	{"1.0+dfsg", "1.0", 1}, // anything else sorts after the end
	{"1.0a", "1.0+", -1},   // letters sort before other characters
	{"1.0.", "1.0+", 1},    // '.' (46) + 256 > '+' (43) + 256
	{"1.0A", "1.0a", -1},   // ASCII order among letters
	{"1.0-1", "1.0-1ubuntu1", -1},
	{"1.0-1ubuntu1", "1.0-1build1", 1},
	{"2.30-1", "2.4-1", 1},
	{"1.0-1~bpo12+1", "1.0-1", -1},
	{"1.2.3", "1.2.3.0", -1},
	{"1a", "1000a", -1},
}

func TestCompare(t *testing.T) {
	for _, tt := range compareTests {
		a, b := mustParse(t, tt.a), mustParse(t, tt.b)
		if got := Compare(a, b); got != tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := Compare(b, a); got != -tt.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tt.b, tt.a, got, -tt.want)
		}
	}
}

// TestCompareMatchesDpkg compares every pair of a version list with
// "dpkg --compare-versions". It is skipped where dpkg is not installed.
func TestCompareMatchesDpkg(t *testing.T) {
	if testing.Short() {
		t.Skip("runs dpkg once per pair")
	}
	dpkg, err := exec.LookPath("dpkg")
	if err != nil {
		t.Skip("dpkg not installed")
	}
	versions := []string{
		"0", "0.0", "00", "1", "1.0", "1.0-0", "1.0-1", "1:0", "1.0~", "1.0~~",
		"1.0~a", "1.0a", "1.0A", "1.0+", "1.0.", "1.00", "1.0.0", "1.0-1~",
		"1.0-1+b1", "1.0-1.1", "1.9", "1.10", "1.10~rc1", "2:0.1", "10", "0a",
		"0~", "1.0-a", "1.0-A", "1.0-0a",
	}
	for i, as := range versions {
		for _, bs := range versions[i+1:] {
			want := 0
			switch {
			case dpkgCompare(t, dpkg, as, "lt", bs):
				want = -1
			case dpkgCompare(t, dpkg, as, "gt", bs):
				want = 1
			}
			if got := Compare(mustParse(t, as), mustParse(t, bs)); got != want {
				t.Errorf("Compare(%q, %q) = %d, dpkg says %d", as, bs, got, want)
			}
		}
	}
}

func dpkgCompare(t *testing.T, dpkg, a, op, b string) bool {
	t.Helper()
	err := exec.Command(dpkg, "--compare-versions", "--", a, op, b).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false
	default:
		t.Fatalf("dpkg --compare-versions %s %s %s: %v", a, op, b, err)
		return false
	}
}

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}
