package main

import (
	"errors"
	"path/filepath"
	"strings"
)

var hostRootView = "/proc/1/root"

// hostFilesystemPath maps an absolute host path to the host filesystem view
// exposed by pid: host. The logical host path remains unchanged in persisted
// jobs and API responses.
func hostFilesystemPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("host filesystem path must be absolute")
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) {
		return hostRootView, nil
	}
	return filepath.Join(hostRootView, strings.TrimPrefix(clean, string(filepath.Separator))), nil
}
