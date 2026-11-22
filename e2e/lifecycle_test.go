//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/deb/debtest"
	"github.com/avesst/garner/internal/publish"
)

// aptSources adds the client file of a suite to the client's sources;
// path is where garner serves it.
func aptSources(url, path, name string) string {
	return fmt.Sprintf(`
perl -MIO::Socket::INET -e '
  my ($host, $port, $path) = $ARGV[0] =~ m{^http://([^:/]+):(\d+)(/.*)$} or die;
  my $s = IO::Socket::INET->new("$host:$port") or die $!;
  print $s "GET $path HTTP/1.0\r\nHost: $host\r\n\r\n";
  local $/; my $r = <$s>; $r =~ s/^.*?\r\n\r\n//s; print $r' %s/%s > /etc/apt/sources.list.d/%s.sources
`, url, path, name)
}

// TestAptLifecycle takes one apt client through what a repository goes
// through over time: install, upgrade, an older version on purpose, a
// removal, a rollback, a second suite, i386 multiarch and a GC that
// deletes files. After each step the client updates and garner verify
// checks the whole tree.
func TestAptLifecycle(t *testing.T) {
	for _, image := range aptImages {
		t.Run(image, func(t *testing.T) {
			e := newEnvWith(t, "deb:\n  architectures: [amd64, i386]\n  suites: [stable, testing]\n",
				"  gc_grace: 0s\n  snapshot_retention: 2\n", "")
			c := startClient(t, image)
			hello := func(v string) { e.upload(debtest.Build("garner-hello", v, "amd64", "hello "+v), "suite=stable") }
			payload := func() string { return c.run("cat /opt/garner-hello/payload") }
			update := func() { c.run("apt-get -q update") }

			// Install.
			hello("1.0")
			e.publish()
			c.run(aptSetup(e.url) + "apt-get -q update\napt-get install -y -q garner-hello")
			if got := payload(); got != "hello 1.0" {
				t.Fatalf("installed payload %q, want hello 1.0", got)
			}
			e.verify()

			// Upgrade. Retention keeps both versions listed.
			hello("1.1")
			e.publish()
			withBoth := currentSnapshot(t, e)
			update()
			c.run("apt-get upgrade -y -q")
			if got := payload(); got != "hello 1.1" {
				t.Fatalf("after upgrade: payload %q, want hello 1.1", got)
			}
			if out := c.run("apt-cache madison garner-hello"); !strings.Contains(out, " 1.0 |") || !strings.Contains(out, " 1.1 |") {
				t.Errorf("both versions should be listed:\n%s", out)
			}
			e.verify()

			// An older version on purpose.
			c.run("apt-get install -y -q --allow-downgrades garner-hello=1.0")
			if got := payload(); got != "hello 1.0" {
				t.Fatalf("after downgrade: payload %q, want hello 1.0", got)
			}

			// Removal: 1.1 disappears from the index.
			e.api(http.MethodDelete, "/api/v1/packages/garner-hello?version=1.1", nil)
			e.publish()
			update()
			if out := c.run("apt-cache madison garner-hello"); strings.Contains(out, " 1.1 |") || !strings.Contains(out, " 1.0 |") {
				t.Errorf("after removing 1.1, apt should list only 1.0:\n%s", out)
			}
			e.verify()

			// Rollback to the snapshot that had both versions.
			e.api(http.MethodPost, "/api/v1/snapshots/"+withBoth+"/rollback", nil)
			update()
			c.run("apt-get upgrade -y -q")
			if got := payload(); got != "hello 1.1" {
				t.Fatalf("after rollback and upgrade: payload %q, want hello 1.1", got)
			}
			e.verify()

			// A second suite: invisible until the client adds it.
			e.upload(debtest.Build("garner-extra", "1.0", "amd64", "extra"), "suite=testing")
			e.publish()
			update()
			if out := c.run("apt-cache policy garner-extra"); strings.Contains(out, "Candidate: 1.0") {
				t.Errorf("a stable client sees a testing package:\n%s", out)
			}
			c.run(aptSources(e.url, "clients/testing/garner.sources", "garner-testing") +
				"apt-get -q update\napt-get install -y -q garner-extra")
			e.verify()

			// i386 multiarch: the same Multi-Arch: same package for both
			// architectures, installed side by side.
			for _, arch := range []string{"amd64", "i386"} {
				e.upload(debtest.BuildWith("garner-lib", "1.0", arch, "lib", "Multi-Arch: same\n"), "suite=stable")
			}
			e.publish()
			c.run("dpkg --add-architecture i386\napt-get -q update\napt-get install -y -q garner-lib:amd64 garner-lib:i386")
			if out := c.run("dpkg-query -W -f '${Package}:${Architecture} ${Version}\\n' 'garner-lib:*'"); !strings.Contains(out, "garner-lib:amd64 1.0") ||
				!strings.Contains(out, "garner-lib:i386 1.0") {
				t.Errorf("both architectures should be installed:\n%s", out)
			}
			e.verify()

			// GC deletes the files of a removed package once no retained
			// snapshot serves it; clients keep working.
			e.api(http.MethodDelete, "/api/v1/packages/garner-extra?distribution=testing", nil)
			e.publish()
			e.publish() // snapshot_retention 2: the snapshots with garner-extra go
			var gc publish.GCResult
			if err := json.Unmarshal(e.api(http.MethodPost, "/api/v1/gc", nil), &gc); err != nil {
				t.Fatal(err)
			}
			if gc.Blobs == 0 || gc.PoolFiles == 0 || len(gc.Snapshots) == 0 {
				t.Errorf("GC deleted nothing: %+v", gc)
			}
			update()
			// madison lists what repositories offer, not the installed copy.
			if out := c.run("apt-cache madison garner-extra"); strings.Contains(out, "garner-extra") {
				t.Errorf("garner-extra still offered after removal:\n%s", out)
			}
			c.run("apt-get install -y -q --reinstall garner-hello")
			e.verify()
		})
	}
}

func currentSnapshot(t *testing.T, e *env) string {
	t.Helper()
	cur, err := e.app.Store.CurrentSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cur.ID
}

// TestDnfLifecycle takes dnf clients through install, upgrade, a removal
// and a rollback, with gpgcheck and repo_gpgcheck on.
// Both clients run each step against one garner; garner verify checks the
// tree after each. GC is format-agnostic and covered by TestAptLifecycle.
func TestDnfLifecycle(t *testing.T) {
	e := newContainerEnv(t)
	upload := func(f string) {
		data, err := os.ReadFile(filepath.Join("..", "internal", "rpm", "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		e.api(http.MethodPut, "/api/v1/upload?release=stable", data)
	}
	verify := func() { checkVerify(t, e.url, "admin", e.token) }
	images := dnfImages
	clients := map[string]*client{}
	for _, image := range images {
		clients[image] = startClient(t, image)
	}
	each := func(script, want string) {
		t.Helper()
		for _, image := range images {
			if out := clients[image].run(script); !strings.Contains(out, want) {
				t.Errorf("%s: output lacks %q:\n%s", image, want, out)
			}
		}
	}
	payload := "cat /opt/garner-hello/payload"

	// Install.
	upload("garner-hello-1.0-1.noarch.rpm")
	e.api(http.MethodPost, "/api/v1/publish", nil)
	each(fmt.Sprintf(`
rm -f /etc/yum.repos.d/*.repo
curl -fsSLo /etc/yum.repos.d/garner.repo %s/garner.repo
dnf -y -q --setopt=install_weak_deps=0 install garner-hello
`, e.url)+payload, "hello 1.0")
	verify()

	// Upgrade.
	upload("garner-hello-2.0-1.x86_64.rpm")
	e.api(http.MethodPost, "/api/v1/publish", nil)
	withBoth := filepath.Base(e.current())
	each("dnf -y -q --refresh upgrade garner-hello\n"+payload, "hello 2.0")
	verify()

	// Removal: 2.0 leaves the repository, and a reinstall gets 1.0. (dnf
	// downgrade keeps the architecture, and 1.0 is noarch.)
	e.api(http.MethodDelete, "/api/v1/packages/garner-hello?version=2.0", nil)
	e.api(http.MethodPost, "/api/v1/publish", nil)
	for _, image := range images {
		if out := clients[image].run("dnf -q --refresh repoquery garner-hello"); strings.Contains(out, "2.0") ||
			!strings.Contains(out, "1.0") {
			t.Errorf("%s: after removing 2.0 the repository should offer only 1.0:\n%s", image, out)
		}
	}
	each("dnf -y -q remove garner-hello\ndnf -y -q --setopt=install_weak_deps=0 install garner-hello\n"+payload, "hello 1.0")
	verify()

	// Rollback to the snapshot with 2.0.
	e.api(http.MethodPost, "/api/v1/snapshots/"+withBoth+"/rollback", nil)
	each("dnf -y -q --refresh upgrade garner-hello\n"+payload, "hello 2.0")
	verify()
}
