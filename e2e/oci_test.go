//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avesst/garner/internal/app"
	"github.com/avesst/garner/internal/auth"
	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/format"
	"github.com/avesst/garner/internal/oci"
	"github.com/avesst/garner/internal/signing"
)

// ociEnv is garner with only the oci section, on a loopback port: the
// host's Docker daemon pushes to and pulls from 127.0.0.1 over plain HTTP
// without configuration.
type ociEnv struct {
	*env
	host      string // 127.0.0.1:port, as image names use it
	dockerCfg string // DOCKER_CONFIG holding the login
}

func newOCIEnv(t *testing.T) *ociEnv {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	cfgPath := filepath.Join(dir, "garner.yaml")
	cfg := fmt.Sprintf("server:\n  public_url: %s\nlog:\n  format: text\n  level: warn\n"+
		"database:\n  url: sqlite://%s/garner.db\nstorage:\n  path: %s\n  gc_grace: 0s\noci: {}\n", url, dir, dir)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := signing.Generate(filepath.Join(dir, "signing", "key.asc"), "e2e repository", ""); err != nil {
		t.Fatal(err)
	}
	a, err := app.Open(context.Background(), cfgPath, "e2e", func(*config.Config) ([]format.Format, error) {
		return []format.Format{oci.Format{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	reg := &oci.Registry{Store: a.Store, Blobs: a.Blobs, Ingester: a.Ingester, Config: a.Config, Root: dir, Log: a.Log}
	a.Live = map[string]http.Handler{oci.Prefix: reg}
	a.Publisher.Collect = append(a.Publisher.Collect, reg.CollectUploads)
	secret, _, err := auth.CreateToken(context.Background(), &a.Store.Queries, "ci", "team", auth.AllScopes)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, app: a, url: url, token: secret, onFile: map[string]func(){}}
	e.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: a.Server().Handler()}}
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	verifyAfter(t, url, "ci", secret)
	o := &ociEnv{env: e, host: ln.Addr().String(), dockerCfg: filepath.Join(dir, "docker")}
	os.MkdirAll(o.dockerCfg, 0o700)
	return o
}

// docker runs the docker CLI with the env's own client configuration, so
// the login stays out of the user's.
func (o *ociEnv) docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+o.dockerCfg)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (o *ociEnv) mustDocker(args ...string) string {
	o.t.Helper()
	out, err := o.docker(args...)
	if err != nil {
		o.t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (o *ociEnv) login(secret string) (string, error) {
	cmd := exec.Command("docker", "login", o.host, "-u", "ci", "--password-stdin")
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+o.dockerCfg)
	cmd.Stdin = strings.NewReader(secret)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestOCIClients pushes with docker and buildx and pulls with docker,
// skopeo (podman's image library), podman and crane.
func TestOCIClients(t *testing.T) {
	o := newOCIEnv(t)
	if out, err := o.login("wrong"); err == nil {
		t.Fatalf("docker login with a bad token succeeded:\n%s", out)
	}
	if out, err := o.login(o.token); err != nil {
		t.Fatalf("docker login: %v\n%s", err, out)
	}

	img := o.host + "/team/busybox:1.37"
	o.mustDocker("pull", "-q", "busybox:1.37")
	o.mustDocker("tag", "busybox:1.37", img)
	t.Cleanup(func() { o.docker("rmi", "-f", img) })
	o.mustDocker("push", img)
	// Indexes with every platform, as buildx pushes them.
	multi := o.host + "/team/alpine:3.20"
	o.mustDocker("buildx", "imagetools", "create", "--tag", multi, "docker.io/library/alpine:3.20")
	o.publish()

	// Anonymous pulls, with the local copy gone.
	o.mustDocker("rmi", "-f", img)
	anon := &ociEnv{env: o.env, host: o.host, dockerCfg: filepath.Join(o.dir, "docker-anon")}
	os.MkdirAll(anon.dockerCfg, 0o700)
	anon.mustDocker("pull", img)
	if out := anon.mustDocker("run", "--rm", img, "echo", "ran-from-garner"); !strings.Contains(out, "ran-from-garner") {
		t.Errorf("docker run: %s", out)
	}
	anon.mustDocker("pull", "--platform", "linux/arm64", multi)
	t.Cleanup(func() { o.docker("rmi", "-f", multi) })

	skopeo := "quay.io/skopeo/stable:v1.20.0"
	sout, err := exec.Command("docker", "run", "--rm", "--network", "host", "--entrypoint", "sh", skopeo, "-ec", "skopeo inspect --tls-verify=false --override-arch arm64 docker://"+multi+
		" && skopeo copy --src-tls-verify=false docker://"+img+" oci:/tmp/copy:latest && skopeo list-tags --tls-verify=false docker://"+o.host+"/team/alpine").CombinedOutput()
	out := string(sout)
	if err != nil || !strings.Contains(out, `"Architecture": "arm64"`) || !strings.Contains(out, `"3.20"`) {
		t.Errorf("skopeo: %v\n%s", err, out)
	}

	pout, err := exec.Command("docker", "run", "--rm", "--network", "host", "--privileged", "quay.io/podman/stable:v5.6",
		"podman", "pull", "--tls-verify=false", img).CombinedOutput()
	if err != nil {
		t.Errorf("podman pull: %v\n%s", err, pout)
	}

	cout, err := exec.Command("docker", "run", "--rm", "--network", "host", "--entrypoint", "crane",
		"gcr.io/go-containerregistry/crane:debug", "manifest", "--insecure", img).CombinedOutput()
	if err != nil || !strings.Contains(string(cout), "schemaVersion") {
		t.Errorf("crane manifest: %v\n%s", err, cout)
	}
}

// TestOCIPushRefusedForOtherOwner checks name ownership with a real
// client: another owner's push fails before its layers are sent.
func TestOCIPushRefusedForOtherOwner(t *testing.T) {
	o := newOCIEnv(t)
	if out, err := o.login(o.token); err != nil {
		t.Fatalf("docker login: %v\n%s", err, out)
	}
	img := o.host + "/team/busybox:1"
	o.mustDocker("pull", "-q", "busybox:1.37")
	o.mustDocker("tag", "busybox:1.37", img)
	t.Cleanup(func() { o.docker("rmi", "-f", img) })
	o.mustDocker("push", img)

	secret, _, err := auth.CreateToken(context.Background(), &o.app.Store.Queries, "other", "someone-else", []string{auth.ScopeUpload})
	if err != nil {
		t.Fatal(err)
	}
	other := &ociEnv{env: o.env, host: o.host, dockerCfg: filepath.Join(o.dir, "docker-other")}
	os.MkdirAll(other.dockerCfg, 0o700)
	cmd := exec.Command("docker", "login", o.host, "-u", "other", "--password-stdin")
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+other.dockerCfg)
	cmd.Stdin = strings.NewReader(secret)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("login as other: %v\n%s", err, out)
	}
	out, err := other.docker("push", img)
	if err == nil || !strings.Contains(out, "owned by team") {
		t.Errorf("push to another owner's name: %v\n%s", err, out)
	}
	o.publish()
}
