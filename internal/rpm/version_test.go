package rpm

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

var vercmpTests = []struct {
	a, b string
	want int
}{
	{"1.0", "1.0", 0},
	{"1.0", "2.0", -1},
	{"2.10", "2.9", 1},
	{"1.010", "1.10", 0},   // leading zeros
	{"1.0", "1.0.1", -1},   // more segments win
	{"1.0a", "1.0", 1},     // anything after the end is newer
	{"1.a", "1.1", -1},     // numeric beats alpha
	{"1.0~rc1", "1.0", -1}, // tilde sorts first
	{"1.0~rc1", "1.0~rc2", -1},
	{"1.0^git1", "1.0", 1},    // caret sorts after the end
	{"1.0^git1", "1.0.1", -1}, // but before anything else
	{"1.0^git1~pre", "1.0^git1", -1},
	{"1_0", "1.0", 0}, // separators are equal
	{"a+b", "a_b", 0},
	{"FC5", "fc4", -1}, // ASCII order
	{"2.0.el9", "2.0.el10", -1},
}

func TestVercmp(t *testing.T) {
	for _, tt := range vercmpTests {
		if got := Vercmp(tt.a, tt.b); got != tt.want {
			t.Errorf("Vercmp(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := Vercmp(tt.b, tt.a); got != -tt.want {
			t.Errorf("Vercmp(%q, %q) = %d, want %d", tt.b, tt.a, got, -tt.want)
		}
	}
}

// TestVercmpMatchesRpm compares every pair of a list with rpm's own
// rpm.vercmp. It is skipped where rpm is not installed.
func TestVercmpMatchesRpm(t *testing.T) {
	rpm, err := exec.LookPath("rpm")
	if err != nil {
		t.Skip("rpm not installed")
	}
	versions := []string{
		"0", "00", "1", "1.0", "1.00", "1.0.0", "1_0", "1.0a", "1.0A", "1.a", "1a",
		"1.0~", "1.0~~", "1.0~rc1", "1.0^", "1.0^git1", "1.0^git1~x", "1.0.1", "1..0",
		"10", "9", "1.10", "1.9", "2.0.el9", "2.0.el10", "a", "b", "~", "^",
	}
	// One rpm invocation evaluates every pair.
	var lua strings.Builder
	for _, a := range versions {
		for _, b := range versions {
			fmt.Fprintf(&lua, "print(rpm.vercmp(%q, %q) .. \" \")\n", a, b)
		}
	}
	out, err := exec.Command(rpm, "--eval", "%{lua:"+lua.String()+"}").Output()
	if err != nil {
		t.Fatalf("rpm --eval: %v", err)
	}
	results := strings.Fields(string(out))
	if len(results) != len(versions)*len(versions) {
		t.Fatalf("rpm printed %d results, want %d", len(results), len(versions)*len(versions))
	}
	k := 0
	for _, a := range versions {
		for _, b := range versions {
			var want int
			fmt.Sscan(results[k], &want)
			k++
			if got := Vercmp(a, b); got != want {
				t.Errorf("Vercmp(%q, %q) = %d, rpm says %d", a, b, got, want)
			}
		}
	}
}
