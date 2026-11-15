package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// BuildID returns a digest of the running executable. Builds that report
// the same version, such as "(devel)" or a rebuild at the same tag, may
// still render snapshots differently.
func BuildID() (string, error) {
	// /proc/self/exe stays the running binary even after a deploy has
	// replaced the file at its path.
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		path, perr := os.Executable()
		if perr != nil {
			return "", perr
		}
		if f, err = os.Open(path); err != nil {
			return "", err
		}
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
