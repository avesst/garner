package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// serveEnv makes the test binary run TestServeChild as a garner serve
// process with its data in the named directory.
const serveEnv = "GARNER_TEST_SERVE_DIR"

// TestServeChild is the child process of TestReloadRightAfterReady.
func TestServeChild(t *testing.T) {
	dir := os.Getenv(serveEnv)
	if dir == "" {
		return
	}
	a, _, err := openIn(t, dir, "deb: {}\n")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := a.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// systemd delivers a reload queued during startup as soon as READY=1
// arrives. garner must handle that SIGHUP, not die of it.
func TestReloadRightAfterReady(t *testing.T) {
	if testing.Short() {
		t.Skip("starts garner processes")
	}
	for i := range 5 {
		dir := t.TempDir()
		sock := filepath.Join(dir, "notify")
		conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
		if err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(dir, "log")
		logFile, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		out := func() string { b, _ := os.ReadFile(logPath); return string(b) }
		cmd := exec.Command(os.Args[0], "-test.run=^TestServeChild$")
		cmd.Env = append(os.Environ(), serveEnv+"="+dir, "NOTIFY_SOCKET="+sock)
		cmd.Stdout, cmd.Stderr = logFile, logFile
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		logFile.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		if err := waitReady(conn, done); err != nil {
			t.Fatalf("round %d: %v\n%s", i, err, out())
		}
		cmd.Process.Signal(syscall.SIGHUP)

		var werr error
		exited := false
		deadline := time.After(10 * time.Second)
	poll:
		for !strings.Contains(out(), "configuration reloaded") {
			select {
			case werr = <-done:
				exited = true
				break poll
			case <-deadline:
				break poll
			case <-time.After(20 * time.Millisecond):
			}
		}
		if !exited {
			cmd.Process.Signal(syscall.SIGTERM)
			werr = <-done
		}
		conn.Close()
		var exit *exec.ExitError
		if errors.As(werr, &exit) {
			if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.Fatalf("round %d: garner died of %v after READY=1\n%s", i, ws.Signal(), out())
			}
		}
		if werr != nil {
			t.Fatalf("round %d: %v\n%s", i, werr, out())
		}
		if exited {
			t.Fatalf("round %d: garner exited before SIGTERM\n%s", i, out())
		}
		if !strings.Contains(out(), "configuration reloaded") {
			t.Fatalf("round %d: SIGHUP did not reload\n%s", i, out())
		}
	}
}

// waitReady returns when the child sends READY=1, or fails when it exits
// first or takes a minute.
func waitReady(conn *net.UnixConn, done <-chan error) error {
	stop := time.Now().Add(time.Minute)
	buf := make([]byte, 4096)
	for time.Now().Before(stop) {
		select {
		case err := <-done:
			return fmt.Errorf("garner exited before READY=1: %v", err)
		default:
		}
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := conn.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			return err
		}
		if strings.Contains(string(buf[:n]), "READY=1") {
			return nil
		}
	}
	return errors.New("no READY=1 within a minute")
}
