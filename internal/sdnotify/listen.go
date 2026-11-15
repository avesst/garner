package sdnotify

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"syscall"
	"time"
)

// Listen returns a TCP listener for addr. Under systemd it reuses the
// listener stored under name by an earlier run of the service, if its
// address still matches; otherwise it drops the stored one, listens anew
// and stores the new one. During a restart systemd holds the socket, so
// connections wait in its queue instead of being refused.
//
// files are the descriptors passed by systemd (from Files); Listen takes
// ownership of those under name.
func Listen(name, addr string, files map[string][]*os.File, log *slog.Logger) (net.Listener, error) {
	stored := files[name]
	delete(files, name)
	var reuse net.Listener
	stale := 0
	for _, f := range stored {
		l, err := net.FileListener(f)
		f.Close()
		if err != nil {
			stale++
			log.Warn("ignoring a stored descriptor that is not a listener", "name", name, "err", err)
			continue
		}
		if reuse == nil && sameAddr(l.Addr(), addr) {
			reuse = l
			continue
		}
		stale++
		log.Info("dropping stored listener; the configured address changed", "name", name, "stored", l.Addr(), "configured", addr)
		l.Close()
	}
	if stale > 0 {
		// FDSTOREREMOVE works by name, so it drops the reused one too;
		// that one is stored again below.
		if err := Remove(name); err != nil {
			log.Warn("could not clear the systemd descriptor store", "name", name, "err", err)
		}
	}
	if reuse != nil {
		log.Info("reusing listener kept by systemd across the restart", "name", name, "addr", reuse.Addr())
		if stale > 0 {
			if err := storeListener(name, reuse); err != nil {
				log.Warn("could not store the listener with systemd", "name", name, "err", err)
			}
		}
		return reuse, nil
	}

	l, err := listenRetry(addr, stale > 0)
	if err != nil {
		return nil, err
	}
	if Enabled() {
		if err := storeListener(name, l); err != nil {
			log.Warn("could not store the listener with systemd; restarts will refuse connections briefly",
				"name", name, "err", err)
		}
	}
	return l, nil
}

// listenRetry listens on addr. After dropping a stored listener, systemd
// may still hold the old socket for a moment, so "address in use" is
// retried for up to two seconds.
func listenRetry(addr string, retry bool) (net.Listener, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		l, err := net.Listen("tcp", addr)
		if err == nil || !retry || !errors.Is(err, syscall.EADDRINUSE) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func storeListener(name string, l net.Listener) error {
	tl, ok := l.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("%T is not a TCP listener", l)
	}
	// Not tl.File().Fd(): Fd switches the descriptor to blocking mode, and
	// the duplicate shares that flag with the listener. A blocking accept
	// is not woken by Close, so shutdown would hang until the next
	// connection arrived.
	rc, err := tl.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = Store(name, int(fd)) }); err != nil {
		return err
	}
	return serr
}

// sameAddr reports whether a bound address is what addr asks for. An
// empty or unspecified host matches a listener on the unspecified
// address.
func sameAddr(bound net.Addr, addr string) bool {
	b, ok := bound.(*net.TCPAddr)
	if !ok {
		return false
	}
	want, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil || want.Port != b.Port {
		return false
	}
	if want.IP == nil || want.IP.IsUnspecified() {
		return b.IP.IsUnspecified()
	}
	return want.IP.Equal(b.IP)
}
