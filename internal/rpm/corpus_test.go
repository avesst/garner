//go:build corpus

// Differential tests against rpm on real packages and version strings.
// They need a corpus directory in GARNER_CORPUS with:
//
//	rpm/*.rpm                     packages from the distributions' mirrors
//	versions/rpm-strings.txt      one version or release string per line
//	versions/rpm-pairs.tsv        two version or release strings per line
//	versions/rpm-evr-pairs.tsv    e1 v1 r1 e2 v2 r2 per line
//
// Fields are tab-separated. Run with:
// GARNER_CORPUS=/path go test -tags corpus -v ./internal/rpm
package rpm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func corpusDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("GARNER_CORPUS")
	if dir == "" {
		t.Skip("GARNER_CORPUS not set")
	}
	return dir
}

func lookTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed", name)
	}
	return p
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("corpus file missing: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestCorpusReadHeader compares ReadHeader's fields with rpm -qp for every
// package in the corpus.
func TestCorpusReadHeader(t *testing.T) {
	dir := corpusDir(t)
	rpmBin := lookTool(t, "rpm")
	files, _ := filepath.Glob(filepath.Join(dir, "rpm", "*.rpm"))
	if len(files) == 0 {
		t.Skip("no .rpm files in the corpus")
	}
	// \x1f separates fields, \x1e packages. rpm prints one record per
	// file, in argument order.
	const qf = "%{NAME}\x1f%{EPOCHNUM}\x1f%{VERSION}\x1f%{RELEASE}\x1f%{ARCH}\x1f" +
		"%{SOURCERPM}\x1f%{SUMMARY}\x1f[%{OBSOLETENAME}\n]\x1e"
	args := append([]string{"-qp", "--nosignature", "--nodigest", "--qf", qf}, files...)
	cmd := exec.Command(rpmBin, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rpm -qp: %v", err)
	}
	records := strings.Split(strings.TrimSuffix(string(out), "\x1e"), "\x1e")
	if len(records) != len(files) {
		t.Fatalf("rpm printed %d records for %d files", len(records), len(files))
	}
	for i, file := range files {
		base := filepath.Base(file)
		r := strings.Split(records[i], "\x1f")
		epoch, _ := strconv.Atoi(r[1])
		var obsoletes []string
		for name := range strings.SplitSeq(strings.TrimSuffix(r[7], "\n"), "\n") {
			if name != "" && nameRE.MatchString(name) && !slices.Contains(obsoletes, name) {
				obsoletes = append(obsoletes, name)
			}
		}
		want := Package{Name: r[0], Epoch: epoch, Version: r[2], Release: r[3], Arch: r[4],
			SourceRPM: r[5], Summary: r[6], Obsoletes: obsoletes}

		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadHeader(f)
		f.Close()
		if err != nil {
			t.Errorf("%s: garner rejects a package rpm reads: %v", base, err)
			continue
		}
		g := *got
		if g.Name != want.Name || g.Epoch != want.Epoch || g.Version != want.Version ||
			g.Release != want.Release || g.Arch != want.Arch || g.SourceRPM != want.SourceRPM {
			t.Errorf("%s: identity\n garner: %s %d:%s-%s.%s %s\n rpm:    %s %d:%s-%s.%s %s", base,
				g.Name, g.Epoch, g.Version, g.Release, g.Arch, g.SourceRPM,
				want.Name, want.Epoch, want.Version, want.Release, want.Arch, want.SourceRPM)
		}
		if g.Summary != want.Summary {
			t.Errorf("%s: summary\n garner: %q\n rpm:    %q", base, g.Summary, want.Summary)
		}
		if !slices.Equal(g.Obsoletes, want.Obsoletes) {
			t.Errorf("%s: obsoletes\n garner: %q\n rpm:    %q", base, g.Obsoletes, want.Obsoletes)
		}
	}
	t.Logf("%d packages compared with rpm -qp", len(files))
}

// TestCorpusVersionStrings checks that ReadHeader's version and release
// syntax accepts every string the distributions ship.
func TestCorpusVersionStrings(t *testing.T) {
	dir := corpusDir(t)
	strs := readLines(t, filepath.Join(dir, "versions", "rpm-strings.txt"))
	rejected := 0
	for _, s := range strs {
		if !verRelRE.MatchString(s) || strings.Contains(s, "..") {
			rejected++
			t.Errorf("garner rejects version or release %q", s)
		}
	}
	t.Logf("%d version and release strings checked, %d rejected", len(strs), rejected)
}

// rpmLua runs a Lua script with rpm's interpreter, which reads the pairs
// file itself, so one rpm process compares every pair.
func rpmLua(t *testing.T, rpmBin, script string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmp.lua")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(rpmBin, "--eval", fmt.Sprintf("%%{lua:dofile(%q)}", path)).Output()
	if err != nil {
		t.Fatalf("rpm --eval: %v", err)
	}
	return strings.Fields(string(out))
}

// TestCorpusVercmp compares Vercmp with rpm.vercmp on version and release
// string pairs.
func TestCorpusVercmp(t *testing.T) {
	dir := corpusDir(t)
	rpmBin := lookTool(t, "rpm")
	pairsFile := filepath.Join(dir, "versions", "rpm-pairs.tsv")
	lines := readLines(t, pairsFile)
	results := rpmLua(t, rpmBin, fmt.Sprintf(`
for line in io.lines(%q) do
  local a, b = line:match("^([^\t]*)\t([^\t]*)$")
  io.write(rpm.vercmp(a, b), "\n")
end`, pairsFile))
	if len(results) != len(lines) {
		t.Fatalf("rpm printed %d results for %d pairs", len(results), len(lines))
	}
	mismatches := 0
	for i, line := range lines {
		a, b, _ := strings.Cut(line, "\t")
		want, _ := strconv.Atoi(results[i])
		if got := Vercmp(a, b); got != want {
			mismatches++
			if mismatches <= 50 {
				t.Errorf("Vercmp(%q, %q) = %d, rpm says %d", a, b, got, want)
			}
		}
	}
	t.Logf("%d pairs compared with rpm.vercmp, %d mismatches", len(lines), mismatches)
}

// TestCorpusCompareEVR compares CompareEVR with rpm.vercmp on full
// epoch:version-release strings.
func TestCorpusCompareEVR(t *testing.T) {
	dir := corpusDir(t)
	rpmBin := lookTool(t, "rpm")
	pairsFile := filepath.Join(dir, "versions", "rpm-evr-pairs.tsv")
	lines := readLines(t, pairsFile)
	results := rpmLua(t, rpmBin, fmt.Sprintf(`
for line in io.lines(%q) do
  local e1, v1, r1, e2, v2, r2 = line:match("^(%%d+)\t([^\t]*)\t([^\t]*)\t(%%d+)\t([^\t]*)\t([^\t]*)$")
  io.write(rpm.vercmp(e1..":"..v1.."-"..r1, e2..":"..v2.."-"..r2), "\n")
end`, pairsFile))
	if len(results) != len(lines) {
		t.Fatalf("rpm printed %d results for %d pairs", len(results), len(lines))
	}
	mismatches := 0
	for i, line := range lines {
		f := strings.Split(line, "\t")
		e1, _ := strconv.Atoi(f[0])
		e2, _ := strconv.Atoi(f[3])
		want, _ := strconv.Atoi(results[i])
		if got := CompareEVR(e1, f[1], f[2], e2, f[4], f[5]); got != want {
			mismatches++
			if mismatches <= 50 {
				t.Errorf("CompareEVR(%s) = %d, rpm says %d", line, got, want)
			}
		}
	}
	t.Logf("%d EVR pairs compared with rpm.vercmp, %d mismatches", len(lines), mismatches)
}
