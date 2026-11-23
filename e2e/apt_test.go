//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/deb/debtest"
)

// aptSetup replaces the image's sources with garner's, fetching the key
// and the .sources file the way the UI tells users to.
func aptSetup(url string) string {
	return fmt.Sprintf(`
rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
apt_fetch() { perl -MIO::Socket::INET -e '
  my ($host, $port, $path) = $ARGV[0] =~ m{^http://([^:/]+):(\d+)(/.*)$} or die;
  my $s = IO::Socket::INET->new("$host:$port") or die $!;
  print $s "GET $path HTTP/1.0\r\nHost: $host\r\n\r\n";
  local $/; my $r = <$s>; $r =~ s/^.*?\r\n\r\n//s; print $r' "$1" > "$2"; }
apt_fetch %[1]s/key.gpg /usr/share/keyrings/garner.gpg
apt_fetch %[1]s/garner.sources /etc/apt/sources.list.d/garner.sources
`, url)
}

// aptImages are the apt clients the tests run: every supported Debian and
// Ubuntu LTS release, oldest first (design section 4).
var aptImages = []string{"debian:bookworm", "debian:trixie", "ubuntu:22.04", "ubuntu:24.04", "ubuntu:26.04"}

// dnfImages are the dnf clients: EL 9 (dnf 4) and 10 from two rebuilds, and
// Fedora (dnf 5).
var dnfImages = []string{"almalinux:9", "almalinux:10", "rockylinux/rockylinux:10", "fedora:44"}

func TestAptInstall(t *testing.T) {
	e := newEnv(t, "")
	e.upload(debtest.Build("garner-hello", "1.0", "amd64", "hello 1.0"), "suite=stable")
	e.publish()
	for _, image := range aptImages {
		t.Run(image, func(t *testing.T) {
			out, err := e.run(image, aptSetup(e.url)+`
apt-get update
apt-get install -y garner-hello
cat /opt/garner-hello/payload
`)
			if err != nil || !strings.Contains(out, "hello 1.0") {
				t.Fatalf("apt failed: %v\n%s", err, out)
			}
		})
	}
}

// TestAptUpdateAcrossPublish publishes a new snapshot after apt has read
// InRelease and before it fetches the indexes InRelease lists. The update
// must succeed through the by-hash history (section 9, step 4).
func TestAptUpdateAcrossPublish(t *testing.T) {
	e := newEnv(t, "")
	e.upload(debtest.Build("garner-hello", "1.0", "amd64", "hello 1.0"), "suite=stable")
	e.publish()
	for i, image := range aptImages {
		t.Run(image, func(t *testing.T) {
			version := fmt.Sprintf("2.%d", i)
			e.upload(debtest.Build("garner-hello", version, "amd64", "hello "+version), "suite=stable")
			e.hook("/dists/stable/InRelease", e.publish)
			out, err := e.run(image, aptSetup(e.url)+`
apt-get -o Debug::Acquire::http=false update 2>&1
apt-cache policy garner-hello
apt-get update
apt-cache policy garner-hello
`)
			if err != nil {
				t.Fatalf("apt-get update failed across a publish: %v\n%s", err, out)
			}
			if strings.Contains(out, "Hash Sum mismatch") || strings.Contains(out, "404") {
				t.Fatalf("apt saw a broken index:\n%s", out)
			}
			if !strings.Contains(out, "Candidate: "+version) {
				t.Errorf("second update did not see %s:\n%s", version, out)
			}
		})
	}
}

// contrib/garner.tmpfiles makes the state directory 0750 so a reverse
// proxy in the garner group can serve public/ from disk. The proxy must
// then be able to read the packages, not only the indexes.
func TestPublicReadableByGroup(t *testing.T) {
	e := newEnv(t, "")
	e.upload(debtest.Build("garner-hello", "1.0", "amd64", "hello 1.0"), "suite=stable")
	e.publish()
	if err := os.Chmod(e.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Run as root only to find the files, then read them as an
	// unprivileged user whose only group is the directory's group.
	cmd := exec.Command("docker", "run", "--rm", "-v", e.dir+":/data:ro,z", "debian:trixie", "sh", "-ec", `
gid=$(stat -c %g /data)
as_proxy() { setpriv --reuid=65534 --regid="$gid" --clear-groups "$@"; }
inrelease=$(find -L /data/public/current/ -name InRelease | head -n1)
deb=$(find -L /data/public/current/ -name '*.deb' | head -n1)
test -n "$inrelease" && test -n "$deb"
ls -lL "$inrelease" "$deb"
as_proxy cat "$inrelease" >/dev/null && echo "proxy reads InRelease"
as_proxy cat "$deb" >/dev/null && echo "proxy reads the .deb"
`)
	out, err := cmd.CombinedOutput()
	if !strings.Contains(string(out), "proxy reads InRelease") {
		t.Fatalf("setup problem, the proxy cannot even read InRelease: %v\n%s", err, out)
	}
	if err != nil || !strings.Contains(string(out), "proxy reads the .deb") {
		t.Fatalf("a proxy in the garner group cannot read published packages: %v\n%s", err, out)
	}
}
