// Package sdnotify implements the parts of systemd's service protocol
// garner uses: readiness notification and the file descriptor store, which
// keeps listening sockets open across restarts. Outside systemd every
// call is a no-op.
package sdnotify

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Notify sends a state such as "READY=1" to systemd, with optional file
// descriptors. Without NOTIFY_SOCKET it does nothing.
func Notify(state string, fds ...int) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	// A plain sendmsg to the socket address, as sd_notify does. The
	// syscall package maps a leading '@' to the abstract namespace.
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	return syscall.Sendmsg(fd, []byte(state), oob, &syscall.SockaddrUnix{Name: path}, 0)
}

// Enabled reports whether garner runs under systemd's notify protocol.
func Enabled() bool { return os.Getenv("NOTIFY_SOCKET") != "" }

// Store hands a descriptor to systemd's file descriptor store under name.
// systemd keeps a duplicate, so the caller may close its own.
func Store(name string, fd int) error {
	if !Enabled() {
		return errors.New("not running under systemd")
	}
	return Notify("FDSTORE=1\nFDNAME="+name, fd)
}

// Remove drops every stored descriptor with the given name.
func Remove(name string) error {
	return Notify("FDSTOREREMOVE=1\nFDNAME=" + name)
}

// listenFDsStart is the first descriptor systemd passes (SD_LISTEN_FDS_START).
const listenFDsStart = 3

// Files returns the descriptors systemd passed to this process, by name,
// and clears the environment variables so child processes do not see
// them. Names come from FDNAME when the descriptors were stored.
func Files() map[string][]*os.File {
	defer func() {
		os.Unsetenv("LISTEN_PID")
		os.Unsetenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_FDNAMES")
	}()
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n <= 0 {
		return nil
	}
	names := strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")
	out := map[string][]*os.File{}
	for i := 0; i < n; i++ {
		fd := listenFDsStart + i
		syscall.CloseOnExec(fd)
		name := "unknown"
		if i < len(names) && names[i] != "" {
			name = names[i]
		}
		out[name] = append(out[name], os.NewFile(uintptr(fd), name))
	}
	return out
}
