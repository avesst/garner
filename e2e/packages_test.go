//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackages installs garner's own .deb and .rpm, as built by
// `just packages`, from the directory in $GARNER_PACKAGES, then checks
// what the maintainer scripts set up and that removal behaves. Run it with
// `just test-packages`.
func TestPackages(t *testing.T) {
	dir := os.Getenv("GARNER_PACKAGES")
	if dir == "" {
		t.Skip("GARNER_PACKAGES not set; run just packages, then just test-packages")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	deb := onePackage(t, dir, "garner_*_amd64.deb")
	rpm := onePackage(t, dir, "garner-*.x86_64.rpm")
	// garner_1.2.3~rc.1_amd64.deb holds garner 1.2.3-rc.1.
	version := strings.ReplaceAll(strings.Split(deb, "_")[1], "~", "-")

	checks := fmt.Sprintf(`
garner version
garner version | grep -qxF 'garner %s'
getent passwd garner
test "$(stat -c '%%U:%%G %%a' /var/lib/garner)" = "garner:garner 750"
grep -q '^ExecStart=/usr/bin/garner serve --config /etc/garner/garner.yaml$' /usr/lib/systemd/system/garner.service
test -f /usr/lib/sysusers.d/garner.conf
test -f /usr/lib/tmpfiles.d/garner.conf
garner config validate /etc/garner/garner.yaml
echo '# local edit' >> /etc/garner/garner.yaml
`, version)

	tests := []struct{ image, script string }{
		{"debian:trixie", aptPackageScript(deb, checks)},
		{"ubuntu:26.04", aptPackageScript(deb, checks)},
		{"almalinux:10", dnfPackageScript(rpm, checks)},
		{"fedora:44", dnfPackageScript(rpm, checks)},
	}
	for _, tc := range tests {
		t.Run(tc.image, func(t *testing.T) {
			out, err := exec.Command("docker", "run", "--rm", "-v", dir+":/pkg:ro,z", tc.image,
				"sh", "-exc", tc.script).CombinedOutput()
			if err != nil || !strings.Contains(string(out), "PACKAGE OK") {
				t.Fatalf("package test failed: %v\n%s", err, out)
			}
		})
	}
}

func onePackage(t *testing.T, dir, pattern string) string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil || len(m) != 1 {
		t.Fatalf("want exactly one %s in %s, found %v", pattern, dir, m)
	}
	return filepath.Base(m[0])
}

// The configuration is a conffile: kept on remove, deleted on purge.
func aptPackageScript(deb, checks string) string {
	return fmt.Sprintf(`
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q --no-install-recommends /pkg/%s
dpkg-query -W -f='${Conffiles}\n' garner | grep -q /etc/garner/garner.yaml
# Listed, not tested on disk: Ubuntu's image does not unpack man pages.
dpkg -L garner | grep -qxF /usr/share/man/man1/garner.1.gz
dpkg -L garner | grep -qxF /usr/share/man/man5/garner.yaml.5.gz
%s
apt-get remove -y -q garner
test ! -e /usr/bin/garner
grep -q '# local edit' /etc/garner/garner.yaml
apt-get purge -y -q garner
test ! -e /etc/garner/garner.yaml
echo PACKAGE OK
`, deb, checks)
}

// The configuration is %%config(noreplace): an edited copy is kept as
// .rpmsave on erase.
func dnfPackageScript(rpm, checks string) string {
	return fmt.Sprintf(`
dnf install -y -q --setopt=install_weak_deps=False /pkg/%s
rpm -qc garner | grep -qxF /etc/garner/garner.yaml
rpm -ql garner | grep -qxF /usr/share/man/man1/garner.1.gz
rpm -ql garner | grep -qxF /usr/share/man/man5/garner.yaml.5.gz
%s
dnf remove -y -q garner
test ! -e /usr/bin/garner
grep -q '# local edit' /etc/garner/garner.yaml.rpmsave
echo PACKAGE OK
`, rpm, checks)
}
