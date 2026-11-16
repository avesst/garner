package server

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/avesst/garner/internal/deb/debtest"
)

// startReal serves f's handler with NewHTTPServer, after the timeouts
// were shortened by the test.
func startReal(t *testing.T, f *fixture) string {
	t.Helper()
	ts := httptest.NewUnstartedServer(f.h)
	ts.Config = NewHTTPServer("", f.h)
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.Listener.Addr().String()
}

func shorten(t *testing.T, v *time.Duration, d time.Duration) {
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

// closedWithin reports whether the server closes conn, or answers and then
// closes it, within d.
func closedWithin(conn net.Conn, d time.Duration) bool {
	conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, conn)
	return err == nil // EOF: closed by the server; a timeout is an error
}

func uploadHeader(f *fixture, size int) string {
	auth := base64.StdEncoding.EncodeToString([]byte("up:" + f.secrets["up"]))
	return fmt.Sprintf("PUT /api/v1/upload?suite=stable HTTP/1.1\r\nHost: x\r\n"+
		"Authorization: Basic %s\r\nContent-Length: %d\r\n\r\n", auth, size)
}

func TestIdleConnectionIsClosed(t *testing.T) {
	shorten(t, &idleTimeout, 300*time.Millisecond)
	f := newFixture(t)
	conn, err := net.Dial("tcp", startReal(t, f))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !closedWithin(conn, 3*time.Second) {
		t.Error("an idle keep-alive connection stayed open")
	}
}

func TestStalledUploadIsCutOff(t *testing.T) {
	shorten(t, &uploadIdleTimeout, 300*time.Millisecond)
	f := newFixture(t)
	conn, err := net.Dial("tcp", startReal(t, f))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, uploadHeader(f, 5000))
	conn.Write(make([]byte, 100)) // then nothing more
	if !closedWithin(conn, 3*time.Second) {
		t.Error("a stalled upload kept its connection open")
	}
}

// An upload that keeps sending may take longer than ReadTimeout.
func TestSlowUploadOutlivesReadTimeout(t *testing.T) {
	shorten(t, &readTimeout, 500*time.Millisecond)
	shorten(t, &uploadIdleTimeout, 400*time.Millisecond)
	f := newFixture(t)
	conn, err := net.Dial("tcp", startReal(t, f))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pkg := debtest.Build("slow", "1.0", "amd64", "s")
	fmt.Fprint(conn, uploadHeader(f, len(pkg)))
	start := time.Now()
	for i, chunk := 0, (len(pkg)+7)/8; i < len(pkg); i += chunk {
		conn.Write(pkg[i:min(i+chunk, len(pkg))])
		time.Sleep(150 * time.Millisecond)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("after %s: %v", time.Since(start).Round(time.Millisecond), err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if time.Since(start) < readTimeout {
		t.Fatalf("upload took %s, not longer than ReadTimeout", time.Since(start))
	}
}
