package main

import (
	"os"
	"reflect"
	"testing"
)

func TestAtomicWriteFileSyncsMetadataBeforeRename(t *testing.T) {
	fs := &recordingAtomicFileSystem{}
	if err := atomicWriteFileWithFS("/config/jobs.json", []byte("{}"), 0o600, FileOwner{UID: 1000, GID: 1000}, fs); err != nil {
		t.Fatal(err)
	}
	want := []string{"mkdir", "create", "write", "chmod", "chown", "file-sync", "file-close", "rename", "open-dir", "dir-sync", "dir-close"}
	if !reflect.DeepEqual(fs.steps, want) {
		t.Fatalf("atomic write sequence = %v, want %v", fs.steps, want)
	}
}

type recordingAtomicFileSystem struct {
	steps []string
}

func (fs *recordingAtomicFileSystem) MkdirAll(string, os.FileMode) error {
	fs.steps = append(fs.steps, "mkdir")
	return nil
}

func (fs *recordingAtomicFileSystem) CreateTemp(string, string) (atomicTemporaryFile, error) {
	fs.steps = append(fs.steps, "create")
	return &recordingTemporaryFile{steps: &fs.steps}, nil
}

func (fs *recordingAtomicFileSystem) Rename(string, string) error {
	fs.steps = append(fs.steps, "rename")
	return nil
}

func (fs *recordingAtomicFileSystem) Remove(string) error {
	fs.steps = append(fs.steps, "remove")
	return nil
}

func (fs *recordingAtomicFileSystem) Open(string) (atomicDirectory, error) {
	fs.steps = append(fs.steps, "open-dir")
	return &recordingAtomicDirectory{steps: &fs.steps}, nil
}

type recordingTemporaryFile struct {
	steps *[]string
}

func (f *recordingTemporaryFile) Name() string { return "/config/.jobs.json.tmp" }
func (f *recordingTemporaryFile) Write([]byte) (int, error) {
	*f.steps = append(*f.steps, "write")
	return 2, nil
}
func (f *recordingTemporaryFile) Sync() error {
	*f.steps = append(*f.steps, "file-sync")
	return nil
}
func (f *recordingTemporaryFile) Chmod(os.FileMode) error {
	*f.steps = append(*f.steps, "chmod")
	return nil
}
func (f *recordingTemporaryFile) Chown(int, int) error {
	*f.steps = append(*f.steps, "chown")
	return nil
}
func (f *recordingTemporaryFile) Close() error {
	*f.steps = append(*f.steps, "file-close")
	return nil
}

type recordingAtomicDirectory struct {
	steps *[]string
}

func (d *recordingAtomicDirectory) Sync() error {
	*d.steps = append(*d.steps, "dir-sync")
	return nil
}
func (d *recordingAtomicDirectory) Close() error {
	*d.steps = append(*d.steps, "dir-close")
	return nil
}
