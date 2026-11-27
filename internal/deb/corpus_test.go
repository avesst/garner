//go:build corpus

// Differential tests against dpkg on real packages and version strings.
// They need a corpus directory in GARNER_CORPUS with:
//
//	deb/*.deb           packages from the distributions' mirrors
//	versions/deb-versions.txt  one dpkg version per line
//	versions/deb-pairs.tsv     two versions per line, tab-separated
//
// Run with: GARNER_CORPUS=/path go test -tags corpus -v ./internal/deb
package deb

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// TestCorpusRead compares what garner reads from each .deb with what
// dpkg-deb reports: the identity garner stores, and every control field
// garner copies into the Packages index.
func TestCorpusRead(t *testing.T) {
	dir := corpusDir(t)
	dpkgDeb := lookTool(t, "dpkg-deb")
	files, _ := filepath.Glob(filepath.Join(dir, "deb", "*.deb"))
	if len(files) == 0 {
		t.Skip("no .deb files in the corpus")
	}
	var fields int
	for _, file := range files {
		base := filepath.Base(file)
		raw, err := exec.Command(dpkgDeb, "--field", file).Output()
		if err != nil {
			t.Errorf("%s: dpkg-deb --field: %v", base, err)
			continue
		}
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		parsed, perr := Format{}.Parse(f)
		var p *Package
		if perr == nil {
			_, _ = f.Seek(0, 0)
			p, perr = Read(f)
		}
		f.Close()
		if perr != nil {
			t.Errorf("%s: garner rejects a package dpkg-deb reads: %v", base, perr)
			continue
		}

		// Field names in the control file, as dpkg-deb prints them.
		var names []string
		for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
			if line != "" && line[0] != ' ' && line[0] != '\t' {
				name, _, _ := strings.Cut(line, ":")
				names = append(names, name)
			}
		}
		var garnerNames []string
		for _, fl := range p.Control {
			garnerNames = append(garnerNames, fl.Name)
		}
		if strings.Join(names, ",") != strings.Join(garnerNames, ",") {
			t.Errorf("%s: field names\n garner: %v\n dpkg:   %v", base, garnerNames, names)
		}

		// dpkg's parsed value of each field, in one call.
		const sep = "\x1e"
		var format strings.Builder
		for _, fl := range p.Control {
			format.WriteString("${" + fl.Name + "}" + sep)
		}
		out, err := exec.Command(dpkgDeb, "--show", "--showformat="+format.String(), file).Output()
		if err != nil {
			t.Errorf("%s: dpkg-deb --show: %v", base, err)
			continue
		}
		values := strings.Split(string(out), sep)
		for i, fl := range p.Control {
			fields++
			if values[i] != fl.Value && corpusRelationFields[strings.ToLower(fl.Name)] &&
				strings.Join(strings.Fields(fl.Value), " ") == values[i] {
				// dpkg prints relationship fields re-rendered from its
				// parse, unfolded; garner keeps the folded original.
				t.Logf("%s: field %s is folded; dpkg prints it unfolded", base, fl.Name)
				continue
			}
			if values[i] != fl.Value {
				t.Errorf("%s: field %s\n garner: %q\n dpkg:   %q", base, fl.Name, fl.Value, values[i])
			}
		}

		// The identity garner stores, against dpkg's view of it.
		out, err = exec.Command(dpkgDeb, "--show",
			"--showformat=${Package}"+sep+"${Version}"+sep+"${Architecture}", file).Output()
		if err != nil {
			t.Errorf("%s: dpkg-deb --show: %v", base, err)
			continue
		}
		id := strings.Split(string(out), sep)
		version := parsed.Version
		if parsed.Epoch != 0 {
			version = strconv.Itoa(parsed.Epoch) + ":" + version
		}
		if parsed.Name != id[0] || version != id[1] || parsed.Arch != id[2] {
			t.Errorf("%s: identity garner %s %s %s, dpkg %s %s %s", base,
				parsed.Name, version, parsed.Arch, id[0], id[1], id[2])
		}
	}
	t.Logf("%d packages, %d control fields compared with dpkg-deb", len(files), fields)
}

var corpusRelationFields = map[string]bool{
	"depends": true, "pre-depends": true, "recommends": true, "suggests": true,
	"enhances": true, "breaks": true, "conflicts": true, "replaces": true,
	"provides": true, "built-using": true, "static-built-using": true,
}

// TestCorpusParseVersion checks that garner accepts every version found in
// the distributions' indexes, and that its stored form round-trips.
func TestCorpusParseVersion(t *testing.T) {
	dir := corpusDir(t)
	versions := readLines(t, filepath.Join(dir, "versions", "deb-versions.txt"))
	rejected := 0
	for _, s := range versions {
		v, err := ParseVersion(s)
		if err != nil {
			rejected++
			t.Errorf("ParseVersion(%q): %v", s, err)
			continue
		}
		if got := v.String(); got != strings.TrimPrefix(s, "0:") {
			t.Errorf("ParseVersion(%q).String() = %q", s, got)
		}
	}
	t.Logf("%d versions parsed, %d rejected", len(versions), rejected)
}

// TestCorpusCompare compares garner's version order with
// "dpkg --compare-versions" for every pair in deb-pairs.tsv whose versions
// garner accepts. dpkg runs once or twice per pair, in parallel.
func TestCorpusCompare(t *testing.T) {
	dir := corpusDir(t)
	dpkg := lookTool(t, "dpkg")
	lines := readLines(t, filepath.Join(dir, "versions", "deb-pairs.tsv"))

	type pair struct {
		a, b string
		got  int
	}
	var pairs []pair
	skipped := 0
	for _, line := range lines {
		as, bs, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("bad pair line %q", line)
		}
		a, errA := ParseVersion(as)
		b, errB := ParseVersion(bs)
		if errA != nil || errB != nil {
			skipped++
			continue
		}
		pairs = append(pairs, pair{as, bs, Compare(a, b)})
	}

	var (
		mu         sync.Mutex
		mismatches int
		wg         sync.WaitGroup
	)
	work := make(chan pair)
	for range runtime.NumCPU() {
		wg.Go(func() {
			for p := range work {
				want := 0
				switch {
				case dpkgCmp(t, dpkg, p.a, "lt", p.b):
					want = -1
				case dpkgCmp(t, dpkg, p.a, "gt", p.b):
					want = 1
				}
				if p.got != want {
					mu.Lock()
					mismatches++
					if mismatches <= 50 {
						t.Errorf("Compare(%q, %q) = %d, dpkg says %d", p.a, p.b, p.got, want)
					}
					mu.Unlock()
				}
			}
		})
	}
	for _, p := range pairs {
		work <- p
	}
	close(work)
	wg.Wait()
	t.Logf("%d pairs compared with dpkg, %d mismatches, %d pairs skipped (garner rejects a version)",
		len(pairs), mismatches, skipped)
}

func dpkgCmp(t *testing.T, dpkg, a, op, b string) bool {
	var stderr bytes.Buffer
	cmd := exec.Command(dpkg, "--compare-versions", "--", a, op, b)
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if stderr.Len() > 0 {
		t.Errorf("dpkg --compare-versions %s %s %s: %s", a, op, b, strings.TrimSpace(stderr.String()))
	}
	switch {
	case err == nil:
		return true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false
	default:
		t.Errorf("dpkg --compare-versions %s %s %s: %v", a, op, b, err)
		return false
	}
}
