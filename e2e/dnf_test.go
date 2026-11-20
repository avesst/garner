//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// imageTag is the garner image built from the repository's Dockerfile.
const imageTag = "garner:e2e"

// containerEnv runs garner from its container image, which carries
// createrepo_c, rpmsign and gpg.
type containerEnv struct {
	t     *testing.T
	dir   string
	url   string
	addr  string
	token string
	name  string // container name of the running server
	user  string // uid:gid the containers run as
}

func buildImage(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "build", "-q", "-t", imageTag, "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
}

func newContainerEnv(t *testing.T) *containerEnv {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	buildImage(t)
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	e := &containerEnv{t: t, dir: dir, url: "http://" + addr, addr: addr,
		name: "garner-e2e-" + filepath.Base(dir), user: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
	e.writeConfig("")
	out, err := exec.Command("docker", "run", "--rm", "--user", e.user, "-v", dir+":/data:z", imageTag,
		"init", "--config", "/data/garner.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("garner init: %v\n%s", err, out)
	}
	m := regexp.MustCompile(`GARNER_TOKEN=admin:(\S+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("no token in init output:\n%s", out)
	}
	e.token = string(m[1])
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", e.name).CombinedOutput()
			t.Logf("garner logs:\n%s", logs)
		}
		exec.Command("docker", "rm", "-f", e.name).Run()
	})
	e.start()
	return e
}

// bothFormats configures deb and rpm.
const bothFormats = "deb:\n  suites: [stable]\nrpm:\n  architectures: [x86_64]\n  releases: [stable]\n"

// writeConfig writes garner.yaml with both formats and extra appended at
// the top level.
func (e *containerEnv) writeConfig(extra string) {
	e.t.Helper()
	e.writeConfigWith(bothFormats, extra)
}

// writeConfigWith writes garner.yaml with the given format sections.
func (e *containerEnv) writeConfigWith(formats, extra string) {
	e.t.Helper()
	cfg := fmt.Sprintf(`server:
  listen: %s
  public_url: %s
log:
  format: text
database:
  url: sqlite:///data/garner.db
storage:
  path: /data
%s%s`, e.addr, e.url, formats, extra)
	if err := os.WriteFile(filepath.Join(e.dir, "garner.yaml"), []byte(cfg), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// start runs garner serve in the background and waits for /readyz. args
// go to docker run before the image, for example "-e", "PATH=...".
func (e *containerEnv) start(args ...string) {
	e.t.Helper()
	run := []string{"run", "-d", "--name", e.name, "--user", e.user, "--network", "host", "-v", e.dir + ":/data:z"}
	run = append(append(run, args...), imageTag, "serve", "--config", "/data/garner.yaml")
	if out, err := exec.Command("docker", run...).CombinedOutput(); err != nil {
		e.t.Fatalf("garner serve: %v\n%s", err, out)
	}
	for i := 0; ; i++ {
		resp, err := http.Get(e.url + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if i > 100 {
			logs, _ := exec.Command("docker", "logs", e.name).CombinedOutput()
			e.t.Fatalf("garner did not become ready: %v\n%s", err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// restart stops the server and starts it again with args (see start).
func (e *containerEnv) restart(args ...string) {
	e.t.Helper()
	if out, err := exec.Command("docker", "stop", "-t", "30", e.name).CombinedOutput(); err != nil {
		e.t.Fatalf("docker stop: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "rm", e.name).CombinedOutput(); err != nil {
		e.t.Fatalf("docker rm: %v\n%s", err, out)
	}
	e.start(args...)
}

func (e *containerEnv) api(method, path string, body []byte) []byte {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.url+path, bytes.NewReader(body))
	req.SetBasicAuth("admin", e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, out)
	}
	return out
}

func TestDnfInstall(t *testing.T) {
	e := newContainerEnv(t)
	for _, f := range []string{"garner-hello-1.0-1.noarch.rpm", "garner-hello-2.0-1.x86_64.rpm"} {
		data, err := os.ReadFile(filepath.Join("..", "internal", "rpm", "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		e.api(http.MethodPut, "/api/v1/upload?release=stable", data)
	}
	e.api(http.MethodPost, "/api/v1/publish", nil)

	for _, image := range []string{"almalinux:10", "fedora:44"} {
		t.Run(image, func(t *testing.T) {
			script := fmt.Sprintf(`
rm -f /etc/yum.repos.d/*.repo
curl -fsSLo /etc/yum.repos.d/garner.repo %[1]s/garner.repo
grep -q repo_gpgcheck=1 /etc/yum.repos.d/garner.repo
dnf -y -q --setopt=install_weak_deps=0 install garner-hello
cat /opt/garner-hello/payload
rpm -q --qf '%%{EPOCH}:%%{VERSION}-%%{RELEASE}.%%{ARCH}\n' garner-hello
rpm -q --qf '%%{SIGPGP:pgpsig}%%{RSAHEADER:pgpsig}\n' garner-hello
`, e.url)
			out, err := exec.Command("docker", "run", "--rm", "--network", "host", image, "sh", "-ec", script).CombinedOutput()
			if err != nil {
				t.Fatalf("dnf failed: %v\n%s", err, out)
			}
			keyID := e.keyID(t)
			for _, want := range []string{"hello 2.0", "3:2.0-1.x86_64", "RSA/SHA256", "Key ID " + keyID} {
				if !strings.Contains(string(out), want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}

	// A repomd.xml whose signature does not match must be refused. The
	// deb Release.gpg is a valid signature by the same key over other data.
	t.Run("bad repomd signature", func(t *testing.T) {
		current, err := os.Readlink(filepath.Join(e.dir, "public", "current"))
		if err != nil {
			t.Fatal(err)
		}
		snap := filepath.Join(e.dir, "public", current)
		sig, err := os.ReadFile(filepath.Join(snap, "deb", "dists", "stable", "Release.gpg"))
		if err != nil {
			t.Fatal(err)
		}
		asc := filepath.Join(snap, "rpm", "stable", "x86_64", "repodata", "repomd.xml.asc")
		os.Chmod(asc, 0o644)
		if err := os.WriteFile(asc, sig, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("docker", "run", "--rm", "--network", "host", "almalinux:10", "sh", "-ec", fmt.Sprintf(`
rm -f /etc/yum.repos.d/*.repo
curl -fsSLo /etc/yum.repos.d/garner.repo %s/garner.repo
dnf -y -q install garner-hello`, e.url)).CombinedOutput()
		if err == nil {
			t.Fatalf("dnf accepted repodata with a bad signature:\n%s", out)
		}
		if !strings.Contains(strings.ToLower(string(out)), "signature") {
			t.Errorf("dnf failed for another reason than the signature:\n%s", out)
		}
		t.Logf("dnf refused as expected:\n%s", out)
	})
}

// keyID returns the long key ID of the signing key garner publishes as
// key.gpg, lowercase as rpm prints it.
func (e *containerEnv) keyID(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(e.url + "/key.gpg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /key.gpg: HTTP %d", resp.StatusCode)
	}
	keys, err := openpgp.ReadKeyRing(resp.Body)
	if err != nil {
		t.Fatalf("key.gpg: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("key.gpg holds %d keys, want 1", len(keys))
	}
	return fmt.Sprintf("%016x", keys[0].PrimaryKey.KeyId)
}
