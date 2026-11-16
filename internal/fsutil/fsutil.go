// Package fsutil has the file operations publishing relies on: durable
// writes, hardlinks with a copy fallback, and syncing a finished tree.
package fsutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// WriteFile writes data to path, creating parent directories, and fsyncs
// the file.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// LinkOrCopy makes dst the same file as src: a hardlink, or a copy when
// hardlinks are not possible (another filesystem, or one without them).
// An existing dst is left alone if it is the same file or has the same
// size; otherwise it is an error.
func LinkOrCopy(src, dst string) error {
	if st, err := os.Stat(dst); err == nil {
		sst, err := os.Stat(src)
		if err != nil {
			return err
		}
		if os.SameFile(st, sst) || st.Size() == sst.Size() {
			return nil
		}
		return fmt.Errorf("%s already exists with different content", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	err := os.Link(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EPERM) &&
		!errors.Is(err, syscall.EMLINK) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// DirSync collects the directories that LinkOrCopy adds entries to, so a
// caller can make many new links durable with one fsync per directory.
type DirSync struct {
	dirs map[string]bool
}

// LinkOrCopy is LinkOrCopy that records the directory dst goes in and the
// parent of every directory it creates. An existing dst adds nothing.
func (s *DirSync) LinkOrCopy(src, dst string) error {
	if _, err := os.Lstat(dst); err != nil {
		if s.dirs == nil {
			s.dirs = map[string]bool{}
		}
		for dir := filepath.Dir(dst); ; dir = filepath.Dir(dir) {
			s.dirs[dir] = true
			if _, err := os.Stat(dir); err == nil || dir == filepath.Dir(dir) {
				break
			}
		}
	}
	return LinkOrCopy(src, dst)
}

// Sync fsyncs every recorded directory and forgets them.
func (s *DirSync) Sync() error {
	for dir := range s.dirs {
		if err := SyncDir(dir); err != nil {
			return err
		}
	}
	s.dirs = nil
	return nil
}

// Symlink creates or atomically replaces a symlink: it creates the new link
// beside the old one and renames it into place.
func Symlink(target, path string) error {
	if cur, err := os.Readlink(path); err == nil && cur == target {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir fsyncs a directory, making renames and new entries in it durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// SyncTree fsyncs every regular file and directory below root. Files
// written with WriteFile are already synced; this covers links and copies
// and the directory entries themselves.
func SyncTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = f.Sync()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
}
