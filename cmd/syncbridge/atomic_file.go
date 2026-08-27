package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type atomicTemporaryFile interface {
	Name() string
	Write([]byte) (int, error)
	Sync() error
	Chmod(os.FileMode) error
	Chown(int, int) error
	Close() error
}

type atomicDirectory interface {
	Sync() error
	Close() error
}

type atomicFileSystem interface {
	MkdirAll(string, os.FileMode) error
	CreateTemp(string, string) (atomicTemporaryFile, error)
	Rename(string, string) error
	Remove(string) error
	Open(string) (atomicDirectory, error)
}

type osAtomicFileSystem struct{}

func (osAtomicFileSystem) MkdirAll(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}
func (osAtomicFileSystem) CreateTemp(dir, pattern string) (atomicTemporaryFile, error) {
	return os.CreateTemp(dir, pattern)
}
func (osAtomicFileSystem) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }
func (osAtomicFileSystem) Remove(path string) error             { return os.Remove(path) }
func (osAtomicFileSystem) Open(path string) (atomicDirectory, error) {
	return os.Open(path)
}

// AtomicWriteFile durably replaces path with data and applies the requested mode
// and owner before the replacement becomes visible.
func AtomicWriteFile(path string, data []byte, mode os.FileMode, owner FileOwner) error {
	return atomicWriteFileWithFS(path, data, mode, owner, osAtomicFileSystem{})
}

func atomicWriteFileWithFS(path string, data []byte, mode os.FileMode, owner FileOwner, fs atomicFileSystem) error {
	dir := filepath.Dir(path)
	if err := fs.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create repository directory: %w", err)
	}

	tmp, err := fs.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = fs.Remove(tmpPath)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if err := tmp.Chown(owner.UID, owner.GID); err != nil {
		cleanup()
		return fmt.Errorf("chown temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = fs.Remove(tmpPath)
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := fs.Rename(tmpPath, path); err != nil {
		_ = fs.Remove(tmpPath)
		return fmt.Errorf("rename temporary file: %w", err)
	}

	d, err := fs.Open(dir)
	if err != nil {
		return fmt.Errorf("open repository directory for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync repository directory: %w", err)
	}
	return nil
}
