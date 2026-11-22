package format

import (
	"fmt"

	"github.com/avesst/garner/internal/config"
	"github.com/avesst/garner/internal/signing"
)

// Severities of a Problem. An error is something a client can trip over;
// a warning is state garner repairs itself, such as files waiting for GC.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Problem is one finding of a repository check.
type Problem struct {
	Severity string `json:"severity"`
	// Check names what was checked, such as "release-hash".
	Check string `json:"check"`
	// Path is the file concerned, relative to storage.path.
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// Errorf returns an error Problem.
func Errorf(check, path, f string, args ...any) Problem {
	return Problem{Severity: SeverityError, Check: check, Path: path, Message: fmt.Sprintf(f, args...)}
}

// Warnf returns a warning Problem.
func Warnf(check, path, f string, args ...any) Problem {
	return Problem{Severity: SeverityWarning, Check: check, Path: path, Message: fmt.Sprintf(f, args...)}
}

// VerifyRequest asks a format to check its part of a snapshot.
type VerifyRequest struct {
	// Dir is the format's directory in the snapshot, such as
	// snapshots/<id>/deb.
	Dir string
	// Previous is the same directory of the snapshot that was current
	// before, whose index files must still be reachable; "" if none.
	Previous string
	// Packages are the snapshot's packages of this format, with the blob
	// and size the snapshot serves them from.
	Packages []*Package
	Config   *config.Config
	// Keys are the keys the snapshot publishes; signatures must verify
	// with them.
	Keys *signing.PublicKeyring
}
