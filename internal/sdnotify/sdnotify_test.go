package sdnotify

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSystemd listens like systemd's notify socket and returns what it
// receives.
func fakeSystemd(t *testing.T) *net.UnixConn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify")
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	return c
}

func TestNotifyAndStore(t *testing.T) {
	c := fakeSystemd(t)
	if err := Notify("READY=1"); err != nil {
		t.Fatal(err)
	}
	buf, oob := make([]byte, 256), make([]byte, 256)
	n, _, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Fatalf("got %q, %v", buf[:n], err)
	}

	l, err := Listen("http", "127.0.0.1:0", nil, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil || string(buf[:n]) != "FDSTORE=1\nFDNAME=http" {
		t.Fatalf("store message %q, %v", buf[:n], err)
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		t.Fatalf("control messages: %v, %v", msgs, err)
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		t.Fatalf("passed fds: %v, %v", fds, err)
	}
	// The passed descriptor is the same listening socket.
	stored, err := net.FileListener(os.NewFile(uintptr(fds[0]), "stored"))
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	if stored.Addr().String() != l.Addr().String() {
		t.Errorf("stored %s, listening %s", stored.Addr(), l.Addr())
	}
}

// Storing a listener must leave it non-blocking: a blocking accept is not
// woken by Close, so shutdown would hang until the next connection
// arrived (the socket stays open in systemd's store).
func TestStoredListenerCloseStopsAccept(t *testing.T) {
	fakeSystemd(t)
	l, err := Listen("http", "127.0.0.1:0", nil, discard())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	addr := l.Addr().String()
	go l.Close() // with a blocking socket, Close waits for the accept
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// A connection releases the blocked accept, as one did in production.
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
		}
		t.Fatal("Accept still blocked after Close")
	}
}

func TestListenReusesMatchingAndDropsStale(t *testing.T) {
	c := fakeSystemd(t)
	old, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := old.(*net.TCPListener).File()
	addr := old.Addr().String()
	old.Close()

	// Same address: reused, nothing sent to systemd.
	l, err := Listen("http", addr, map[string][]*os.File{"http": {f}}, discard())
	if err != nil {
		t.Fatal(err)
	}
	if l.Addr().String() != addr {
		t.Errorf("reused %s, want %s", l.Addr(), addr)
	}
	c.SetReadDeadline(deadlineSoon())
	if n, _, _, _, err := c.ReadMsgUnix(make([]byte, 64), nil); err == nil {
		t.Errorf("reuse sent a message to systemd (%d bytes)", n)
	}

	// Another address: the stored one is removed and a new one stored.
	f2, _ := l.(*net.TCPListener).File()
	l.Close()
	l2, err := Listen("http", "127.0.0.1:0", map[string][]*os.File{"http": {f2}}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	var got []string
	for range 2 {
		buf := make([]byte, 64)
		c.SetReadDeadline(deadlineSoon())
		n, _, _, _, err := c.ReadMsgUnix(buf, make([]byte, 64))
		if err != nil {
			break
		}
		got = append(got, string(buf[:n]))
	}
	if strings.Join(got, "|") != "FDSTOREREMOVE=1\nFDNAME=http|FDSTORE=1\nFDNAME=http" {
		t.Errorf("messages = %q", got)
	}
}

func TestSameAddr(t *testing.T) {
	tcp := func(s string) net.Addr { a, _ := net.ResolveTCPAddr("tcp", s); return a }
	for _, tt := range []struct {
		bound, want string
		ok          bool
	}{
		{"0.0.0.0:8080", ":8080", true},
		{"[::]:8080", ":8080", true},
		{"127.0.0.1:8080", "127.0.0.1:8080", true},
		{"0.0.0.0:8080", "127.0.0.1:8080", false},
		{"127.0.0.1:8080", ":8080", false},
		{"127.0.0.1:8080", "127.0.0.1:9090", false},
	} {
		if got := sameAddr(tcp(tt.bound), tt.want); got != tt.ok {
			t.Errorf("sameAddr(%s, %s) = %v", tt.bound, tt.want, got)
		}
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func deadlineSoon() time.Time { return time.Now().Add(100 * time.Millisecond) }
