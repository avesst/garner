//go:build e2e

package e2e

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avesst/garner/internal/signing"
)

// rpmFixture is the x86_64 test package, version 3:2.0-1.
const rpmFixture = "garner-hello-2.0-1.x86_64.rpm"

func (e *containerEnv) uploadRPM() {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "internal", "rpm", "testdata", rpmFixture))
	if err != nil {
		e.t.Fatal(err)
	}
	e.api(http.MethodPut, "/api/v1/upload?release=stable", data)
}

// dnfInstall installs garner-hello with dnf and prints the signing key of
// the installed package.
func (e *containerEnv) dnfInstall() (string, error) {
	script := fmt.Sprintf(`
rm -f /etc/yum.repos.d/*.repo
curl -fsSLo /etc/yum.repos.d/garner.repo %[1]s/garner.repo
dnf -y -q --setopt=install_weak_deps=0 install garner-hello
cat /opt/garner-hello/payload
rpm -q --qf '%%{RSAHEADER:pgpsig}\n' garner-hello
`, e.url)
	out, err := exec.Command("docker", "run", "--rm", "--network", "host", "almalinux:10", "sh", "-ec", script).CombinedOutput()
	return string(out), err
}

// current returns the snapshot public/current points to.
func (e *containerEnv) current() string {
	e.t.Helper()
	cur, err := os.Readlink(filepath.Join(e.dir, "public", "current"))
	if err != nil {
		e.t.Fatal(err)
	}
	return cur
}

// waitPublish waits until public/current moves away from prev, so the
// publish garner schedules at start has run.
func (e *containerEnv) waitPublish(prev string) {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for e.current() == prev {
		if time.Now().After(deadline) {
			e.t.Fatalf("no publish within 60s; current is still %s", prev)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitLog reports whether the server's log contains substr within d.
func (e *containerEnv) waitLog(substr string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		logs, _ := exec.Command("docker", "logs", e.name).CombinedOutput()
		if strings.Contains(string(logs), substr) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// garner starts once without the rpm tools, so rpm is disabled, and then
// with them again. The rpm packages must still be there: a missing tool
// is not a request to delete a format's packages.
func TestRpmSurvivesStartWithoutTools(t *testing.T) {
	e := newContainerEnv(t)
	e.uploadRPM()
	e.api(http.MethodPost, "/api/v1/publish", nil)
	if out, err := e.dnfInstall(); err != nil || !strings.Contains(out, "hello 2.0") {
		t.Fatalf("setup: dnf failed before the restart: %v\n%s", err, out)
	}

	// createrepo_c, rpmsign and gpg live in /usr/bin in the image.
	prev := e.current()
	e.restart("-e", "PATH=/usr/local/bin")
	logs, _ := exec.Command("docker", "logs", e.name).CombinedOutput()
	if !strings.Contains(string(logs), "rpm support disabled") {
		t.Fatalf("setup: rpm was not disabled:\n%s", logs)
	}
	e.waitPublish(prev) // the start-up publish: the format set changed

	prev = e.current()
	e.restart()
	e.waitPublish(prev)
	if out, err := e.dnfInstall(); err != nil || !strings.Contains(out, "hello 2.0") {
		t.Fatalf("rpm packages gone after one start without the rpm tools: %v\n%s", err, out)
	}
}

// The documented key rotation done with restarts instead
// of SIGHUP, as a GitOps rollout does it. After the new key is promoted,
// the served rpms must carry its signature.
func TestRpmResignedAfterKeyPromotionAndRestart(t *testing.T) {
	e := newContainerEnv(t)
	e.uploadRPM()
	e.api(http.MethodPost, "/api/v1/publish", nil)
	oldID := e.keyID(t)
	oldFiles := e.rpmPoolFiles()
	if len(oldFiles) == 0 {
		t.Fatal("no rpm in the pool after the first publish")
	}

	fp, err := signing.Generate(filepath.Join(e.dir, "signing", "new.asc"), "e2e repository new", "")
	if err != nil {
		t.Fatal(err)
	}
	newID := strings.ToLower(fp[len(fp)-16:])

	// Step 1: add the new key second.
	e.writeConfig("signing:\n  keys:\n    - key_file: /data/signing/key.asc\n    - key_file: /data/signing/new.asc\n")
	prev := e.current()
	e.restart()
	e.waitPublish(prev)

	// Step 3: move the new key first.
	e.writeConfig("signing:\n  keys:\n    - key_file: /data/signing/new.asc\n    - key_file: /data/signing/key.asc\n")
	e.restart()
	if !e.waitLog("re-signed packages", 60*time.Second) {
		logs, _ := exec.Command("docker", "logs", e.name).CombinedOutput()
		t.Fatalf("no re-sign after promoting the new key and restarting:\n%s", logs)
	}
	e.api(http.MethodPost, "/api/v1/publish", nil)

	out, err := e.dnfInstall()
	if err != nil {
		t.Fatalf("dnf failed after the key promotion: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Key ID "+newID) {
		t.Fatalf("rpm still signed by the old key %s, want the new key %s:\n%s", oldID, newID, out)
	}

	// Snapshots from before the promotion are retained and still serve
	// the rpm signed with the old key.
	e.api(http.MethodPost, "/api/v1/gc", nil)
	for _, f := range oldFiles {
		if _, err := os.Stat(filepath.Join(e.dir, "pool", "rpm", f)); err != nil {
			t.Errorf("GC removed %s, which retained snapshots still serve: %v", f, err)
		}
	}
}

// rpmPoolFiles returns the rpm files in the pool, relative to it.
func (e *containerEnv) rpmPoolFiles() []string {
	e.t.Helper()
	root := filepath.Join(e.dir, "pool", "rpm")
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".rpm") {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}
