//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// status returns the HTTP status of a GET of path.
func (e *containerEnv) status(path string) int {
	e.t.Helper()
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(e.url + path)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
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

// garner starts once without the rpm section, so rpm is disabled, and then
// with it again. The rpm packages must still be there: disabling a format
// is not a request to delete its packages.
func TestRpmSurvivesSectionRemoval(t *testing.T) {
	e := newContainerEnv(t)
	e.uploadRPM()
	e.api(http.MethodPost, "/api/v1/publish", nil)
	if out, err := e.dnfInstall(); err != nil || !strings.Contains(out, "hello 2.0") {
		t.Fatalf("setup: dnf failed before the restart: %v\n%s", err, out)
	}

	prev := e.current()
	e.writeConfigWith("deb:\n  suites: [stable]\n", "")
	e.restart()
	e.waitPublish(prev) // the start-up publish: the format set changed
	if code := e.status("/rpm/stable/x86_64/repodata/repomd.xml"); code != http.StatusNotFound {
		t.Fatalf("rpm still served without its section: HTTP %d", code)
	}
	if !e.waitLog("packages of a format that is not configured are kept but not served", 30*time.Second) {
		t.Fatal("no warning about the stored rpm packages that are no longer served")
	}

	prev = e.current()
	e.writeConfig("")
	e.restart()
	e.waitPublish(prev)
	if out, err := e.dnfInstall(); err != nil || !strings.Contains(out, "hello 2.0") {
		t.Fatalf("rpm packages gone after one start without the rpm section: %v\n%s", err, out)
	}
}

// A configured rpm without its tools is an error, not a silent fallback
// to deb only.
func TestRpmConfiguredWithoutToolsRefusesToStart(t *testing.T) {
	e := newContainerEnv(t)
	if out, err := exec.Command("docker", "rm", "-f", e.name).CombinedOutput(); err != nil {
		t.Fatalf("docker rm: %v\n%s", err, out)
	}
	// createrepo_c, rpmsign and gpg live in /usr/bin in the image. Killing
	// the docker CLI on timeout would leave the container running, so it
	// is named and removed explicitly.
	name := e.name + "-notools"
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name, "--user", e.user,
		"--network", "host", "-v", e.dir+":/data:z", "-e", "PATH=/usr/local/bin", imageTag,
		"serve", "--config", "/data/garner.yaml").CombinedOutput()
	if err == nil || ctx.Err() != nil {
		exec.Command("docker", "rm", "-f", name).Run() // free the port for the start below
		t.Fatalf("garner started without the rpm tools (err %v):\n%s", err, out)
	}
	if !strings.Contains(string(out), "rpm is configured: missing tools: createrepo_c") ||
		!strings.Contains(string(out), "install them, or remove the rpm section") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	e.start() // and it starts again with them
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

// Adding a signing subkey to the signing key, the usual way to roll a
// subkey that is about to expire, keeps the primary fingerprint but
// changes the key that signs. The stored rpm packages must be re-signed
// with the new subkey, or they break once the old one is retired.
func TestRpmResignedAfterSigningSubkeyAdded(t *testing.T) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg not installed")
	}
	e := newContainerEnv(t)
	e.uploadRPM()
	e.api(http.MethodPost, "/api/v1/publish", nil)
	oldFiles := e.rpmPoolFiles()

	keyFile := filepath.Join(e.dir, "signing", "key.asc")
	home := filepath.Join(t.TempDir(), "gnupg")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run() })
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(gpg, append([]string{"--homedir", home, "--batch",
			"--pinentry-mode", "loopback", "--passphrase", ""}, args...)...).Output()
		if err != nil {
			stderr := ""
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			t.Fatalf("gpg %v: %v\n%s", args, err, stderr)
		}
		return string(out)
	}
	run("--import", keyFile)
	colons := func() [][]string {
		var recs [][]string
		for _, line := range strings.Split(run("--with-colons", "--list-keys"), "\n") {
			recs = append(recs, strings.Split(line, ":"))
		}
		return recs
	}
	var primary string
	for _, f := range colons() {
		if f[0] == "fpr" {
			primary = f[9]
			break
		}
	}
	run("--quick-add-key", primary, "rsa2048", "sign", "never")
	var subID string // the last subkey listed is the one just added
	for _, f := range colons() {
		if f[0] == "sub" {
			subID = strings.ToLower(f[4])
		}
	}
	if err := os.WriteFile(keyFile, []byte(run("--armor", "--export-secret-keys", primary)), 0o600); err != nil {
		t.Fatal(err)
	}

	e.restart()
	if !e.waitLog("re-signed packages", 60*time.Second) {
		t.Fatal("no re-sign after adding a signing subkey and restarting")
	}
	e.api(http.MethodPost, "/api/v1/publish", nil)

	var resigned []string
	for _, f := range e.rpmPoolFiles() {
		if !slices.Contains(oldFiles, f) {
			resigned = append(resigned, f)
		}
	}
	if len(resigned) == 0 {
		t.Fatal("no re-signed rpm in the pool")
	}
	for _, f := range resigned {
		out, _ := exec.Command("docker", "run", "--rm", "--user", e.user, "--entrypoint", "rpm",
			"-v", e.dir+":/data:z", imageTag, "-Kv", "/data/pool/rpm/"+f).CombinedOutput()
		if !strings.Contains(strings.ToLower(string(out)), subID[len(subID)-8:]) {
			t.Errorf("%s is not signed by the new subkey %s:\n%s", f, subID, out)
		}
	}
	if out, err := e.dnfInstall(); err != nil {
		t.Fatalf("dnf failed after adding the subkey: %v\n%s", err, out)
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

// i686 packages are multilib: dnf on x86_64 installs them next to the
// x86_64 package of the same version, from the x86_64 repository.
func TestDnfMultilib(t *testing.T) {
	e := newContainerEnv(t)
	e.writeConfigWith("deb:\n  suites: [stable]\nrpm:\n  architectures: [x86_64, i686]\n  releases: [stable]\n", "")
	e.restart()
	for _, f := range []string{"garner-hello-2.0-1.x86_64.rpm", "garner-hello-2.0-1.i686.rpm"} {
		data, err := os.ReadFile(filepath.Join("..", "internal", "rpm", "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		e.api(http.MethodPut, "/api/v1/upload?release=stable", data)
	}
	e.api(http.MethodPost, "/api/v1/publish", nil)
	if code := e.status("/rpm/stable/i686/repodata/repomd.xml"); code != http.StatusNotFound {
		t.Errorf("an i686 repository is served: HTTP %d", code)
	}
	for _, image := range dnfImages {
		t.Run(image, func(t *testing.T) {
			script := fmt.Sprintf(`
rm -f /etc/yum.repos.d/*.repo
curl -fsSLo /etc/yum.repos.d/garner.repo %s/garner.repo
dnf -y -q --setopt=install_weak_deps=0 install garner-hello.x86_64 garner-hello.i686
rpm -q --qf '%%{NAME}-%%{EPOCH}:%%{VERSION}-%%{RELEASE}.%%{ARCH}\n' garner-hello | sort
`, e.url)
			out, err := exec.Command("docker", "run", "--rm", "--network", "host", image, "sh", "-ec", script).CombinedOutput()
			if err != nil {
				t.Fatalf("dnf failed: %v\n%s", err, out)
			}
			for _, want := range []string{"garner-hello-3:2.0-1.i686", "garner-hello-3:2.0-1.x86_64"} {
				if !strings.Contains(string(out), want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}
