package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
)

const maxUpdateProgressBytes = 1 << 20

// UpdateProgress is a read-only, externally supplied maintenance snapshot.
// It deliberately cannot execute or modify jobs, containers or host services.
type UpdateProgress struct {
	SchemaVersion     int                 `json:"schemaVersion"`
	Title             string              `json:"title"`
	UpdatedAt         string              `json:"updatedAt"`
	BackupLocalFrozen bool                `json:"backupLocalFrozen"`
	Counts            map[string]int      `json:"counts"`
	Rows              []UpdateProgressRow `json:"rows"`
}

type UpdateProgressRow struct {
	ID        string  `json:"id"`
	Target    string  `json:"target"`
	Kind      string  `json:"kind"`
	Component string  `json:"component"`
	Image     string  `json:"image"`
	Status    string  `json:"status"`
	Note      string  `json:"note"`
	CheckedAt *string `json:"checkedAt"`
}

func (a *App) handleUpdateProgress(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	empty := UpdateProgress{SchemaVersion: 1, Counts: map[string]int{}, Rows: []UpdateProgressRow{}}
	if a == nil || a.UpdateProgressPath == "" {
		writeJSONStatus(w, http.StatusOK, empty)
		return
	}
	st, err := os.Lstat(a.UpdateProgressPath)
	if errors.Is(err, os.ErrNotExist) {
		writeJSONStatus(w, http.StatusOK, empty)
		return
	}
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxUpdateProgressBytes {
		writeAPIError(w, http.StatusServiceUnavailable, "progress_unavailable", "maintenance snapshot unavailable")
		return
	}
	contents, err := os.ReadFile(a.UpdateProgressPath)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "progress_unavailable", "maintenance snapshot unavailable")
		return
	}
	var progress UpdateProgress
	if err := json.Unmarshal(contents, &progress); err != nil || progress.SchemaVersion != 1 || len(progress.Rows) > 1000 {
		writeAPIError(w, http.StatusServiceUnavailable, "progress_invalid", "maintenance snapshot is invalid")
		return
	}
	if progress.Rows == nil {
		progress.Rows = []UpdateProgressRow{}
	}
	if progress.Counts == nil {
		progress.Counts = map[string]int{}
	}
	writeJSONStatus(w, http.StatusOK, progress)
}
