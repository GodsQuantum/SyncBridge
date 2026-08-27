package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var backupDirectoryPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(?:-[0-9]+)?$`)

// handleRunTerminalEffects preserves the user-visible terminal behavior that
// existed before RunService became the single execution owner. Dry-runs never
// mutate backup retention or emit notifications.
func handleRunTerminalEffects(job Job, run RunSnapshot) {
	if run.DryRun {
		return
	}
	if run.Status == RunSucceeded && job.Action.Type == ActionSync {
		sync := job.Action.Sync
		if sync.Backup && sync.BackupKeep > 0 {
			if err := rotateBackupsHost(sync.Dest, sync.BackupKeep); err != nil {
				fmt.Printf("[backup] retention failed for job %d: %v\n", job.ID, err)
			}
		}
	}
	status := "error"
	switch run.Status {
	case RunSucceeded:
		status = "ok"
	case RunKilled:
		status = "stopped"
	case RunSkippedOverlap:
		return
	}
	notifyRun(job.Name, status)
}

// rotateBackupsHost keeps the newest timestamped SyncBridge backup directories
// on the host filesystem. Unrelated directories are never removed.
func rotateBackupsHost(destination string, keep int) error {
	if keep <= 0 {
		return nil
	}
	base, err := hostFilesystemPath(filepath.Join(destination, ".sb-backup"))
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && backupDirectoryPattern.MatchString(entry.Name()) {
			dirs = append(dirs, entry.Name())
		}
	}
	if len(dirs) <= keep {
		return nil
	}
	sort.Strings(dirs)
	for _, name := range dirs[:len(dirs)-keep] {
		path := filepath.Join(base, name)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove backup %s: %w", name, err)
		}
	}
	return nil
}
