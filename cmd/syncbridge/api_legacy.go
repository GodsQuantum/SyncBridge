package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func legacyJobView(job Job) Job {
	view := cloneJob(job)
	view.SchemaVersion = 0
	view.Disabled = !job.Enabled
	view.Backend = string(job.Scheduler.Owner)
	view.Timeout = job.Execution.TimeoutSeconds
	switch job.Action.Type {
	case ActionCommand:
		view.Kind = "command"
		view.Command = job.Action.Command
	case ActionSync:
		view.Kind = "sync"
		view.Engine = job.Action.Sync.Engine
		view.Source = job.Action.Sync.Source
		view.Dest = job.Action.Sync.Dest
		view.Mode = job.Action.Sync.Mode
		view.Compare = job.Action.Sync.Compare
		view.Bwlimit = job.Action.Sync.Bwlimit
		view.Backup = job.Action.Sync.Backup
		view.BackupKeep = job.Action.Sync.BackupKeep
		view.MaxDel = job.Action.Sync.MaxDel
		view.SkipNew = job.Action.Sync.SkipNew
		view.SysBackup = job.Action.Sync.SysBackup
		view.Exclude = job.Action.Sync.Exclude
	case ActionScript:
		view.Kind = "command"
		view.Command = job.Action.ScriptPath
	}
	if view.Backend == "" {
		view.Backend = "syncbridge"
	}
	if view.Compare == "" {
		view.Compare = "time"
	}
	return view
}

func legacyToV2(in Job) (Job, error) {
	defaults(&in)
	out := cloneJob(in)
	out.SchemaVersion = 2
	out.ID = 0
	out.Revision = 0
	out.Enabled = !in.Disabled
	out.Execution.TimeoutSeconds = in.Timeout
	if out.Execution.Overlap == "" {
		out.Execution.Overlap = "skip"
	}
	out.Scheduler.Owner = SchedulerSyncBridge
	if in.Backend == "system" {
		out.Scheduler.Owner = SchedulerSystem
	}
	if in.Kind == "command" {
		out.Action = Action{Type: ActionCommand, Command: in.Command}
	} else {
		out.Action = Action{Type: ActionSync, Sync: SyncAction{
			Engine: in.Engine, Source: in.Source, Dest: in.Dest, Mode: in.Mode,
			Compare: in.Compare, Bwlimit: in.Bwlimit, Backup: in.Backup, BackupKeep: in.BackupKeep,
			MaxDel: in.MaxDel, SkipNew: in.SkipNew, SysBackup: in.SysBackup, Exclude: in.Exclude,
		}}
	}
	if errText := validateJob(&in); errText != "" {
		return Job{}, errors.New(errText)
	}
	if err := validateV2Job(out); err != nil {
		return Job{}, err
	}
	out.NeedsReview = false
	return out, nil
}

func (a *App) handleLegacyJobs(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.Jobs == nil {
		http.Error(w, "service unavailable", 500)
		return
	}
	switch r.Method {
	case http.MethodGet:
		jobs := a.Jobs.List()
		out := make([]Job, 0, len(jobs))
		for _, job := range jobs {
			out = append(out, legacyJobView(job))
		}
		writeJSON(w, out)
	case http.MethodPost:
		var in Job
		if err := decodeStrictJSON(w, r, &in); err != nil {
			http.Error(w, "json", 400)
			return
		}
		job, err := legacyToV2(in)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		created, err := a.Jobs.Create(job)
		if err != nil {
			http.Error(w, "persist", 500)
			return
		}
		_ = a.reconcileTriggers()
		writeJSON(w, legacyJobView(created))
	default:
		http.Error(w, "méthode", 405)
	}
}

func (a *App) handleLegacyJobByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "/")
	parts := strings.Split(rest, "/")
	id, err := strconv.Atoi(parts[0])
	if err != nil || id <= 0 {
		http.Error(w, "id", 400)
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "run":
			a.legacyRun(w, r, id)
			return
		case "stream":
			a.legacyStream(w, r, id)
			return
		case "history":
			if r.Method != http.MethodGet {
				http.Error(w, "méthode", 405)
				return
			}
			apiJobHistory(w, id)
			return
		case "clone":
			a.legacyClone(w, r, id)
			return
		case "toggle":
			a.legacyToggle(w, r, id)
			return
		case "kill":
			a.legacyKill(w, r, id)
			return
		}
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		current, ok := a.Jobs.Get(id)
		if !ok {
			http.Error(w, "introuvable", 404)
			return
		}
		var in Job
		if err := decodeStrictJSON(w, r, &in); err != nil {
			http.Error(w, "json", 400)
			return
		}
		candidate, err := legacyToV2(in)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		candidate.Enabled = current.Enabled
		candidate.Disabled = !current.Enabled
		updated, err := a.Jobs.Update(id, current.Revision, candidate)
		if err != nil {
			http.Error(w, "conflit", 409)
			return
		}
		_ = a.reconcileTriggers()
		writeJSON(w, legacyJobView(updated))
	case http.MethodDelete:
		current, ok := a.Jobs.Get(id)
		if !ok {
			http.Error(w, "introuvable", 404)
			return
		}
		if a.jobHasActiveRun(id) {
			http.Error(w, "run actif", 409)
			return
		}
		if err := a.Jobs.Delete(id, current.Revision); err != nil {
			http.Error(w, "conflit", 409)
			return
		}
		removeHistory(id)
		_ = a.reconcileTriggers()
		writeJSON(w, map[string]int{"deleted": id})
	default:
		http.Error(w, "méthode", 405)
	}
}

func (a *App) legacyRun(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "méthode", 405)
		return
	}
	if a.Runs == nil {
		http.Error(w, "service unavailable", 500)
		return
	}
	job, ok := a.Jobs.Get(id)
	if !ok {
		http.Error(w, "introuvable", 404)
		return
	}
	dry := r.URL.Query().Get("dry") == "1"
	run, err := a.Runs.Start(r.Context(), StartRunInput{JobID: id, Revision: job.Revision, Origin: RunOriginManual, DryRun: dry})
	if errors.Is(err, ErrOverlap) {
		http.Error(w, "déjà en cours", 409)
		return
	}
	if err != nil {
		http.Error(w, "échec lancement", 500)
		return
	}
	writeJSON(w, map[string]any{"started": id, "dry": dry, "runId": run.ID})
}

func (a *App) legacyClone(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "méthode", 405)
		return
	}
	orig, ok := a.Jobs.Get(id)
	if !ok {
		http.Error(w, "introuvable", 404)
		return
	}
	cp := cloneJob(orig)
	cp.ID = 0
	cp.Revision = 0
	cp.Name = orig.Name + " (copie)"
	cp.Enabled = false
	cp.Disabled = true
	cp.LastRun = ""
	cp.LastStat = ""
	created, err := a.Jobs.Create(cp)
	if err != nil {
		http.Error(w, "persist", 500)
		return
	}
	_ = a.reconcileTriggers()
	writeJSON(w, legacyJobView(created))
}
func (a *App) legacyToggle(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "méthode", 405)
		return
	}
	job, ok := a.Jobs.Get(id)
	if !ok {
		http.Error(w, "introuvable", 404)
		return
	}
	job.Enabled = !job.Enabled
	job.Disabled = !job.Enabled
	updated, err := a.Jobs.Update(id, job.Revision, job)
	if err != nil {
		http.Error(w, "conflit", 409)
		return
	}
	_ = a.reconcileTriggers()
	writeJSON(w, map[string]any{"id": id, "disabled": !updated.Enabled, "revision": updated.Revision})
}
func (a *App) legacyKill(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodPost {
		http.Error(w, "méthode", 405)
		return
	}
	if a.Runs == nil {
		http.Error(w, "service unavailable", 500)
		return
	}
	runs := a.Runs.List(RunFilter{JobID: id})
	killed := false
	for _, run := range runs {
		if !isTerminalRunStatus(run.Status) {
			if err := a.Runs.Stop(r.Context(), run.ID); err == nil || errors.Is(err, ErrRunNotActive) {
				killed = true
			}
			break
		}
	}
	writeJSON(w, map[string]any{"id": id, "killed": killed})
}

func (a *App) legacyStream(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodGet {
		http.Error(w, "méthode", 405)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}
	if a.Runs == nil {
		http.Error(w, "service unavailable", 500)
		return
	}
	runs := a.Runs.List(RunFilter{JobID: id, Limit: 1})
	if len(runs) == 0 {
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
		fl.Flush()
		return
	}
	target := runs[0].ID
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	sub := a.Runs.Subscribe(0)
	defer sub.Close()
	seenLog := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-sub.Events:
			if !open {
				return
			}
			if ev.Run.ID != target {
				continue
			}
			if ev.Kind == RunEventLog {
				line := strings.TrimRight(string(ev.Log), "\r\n")
				if line == "" {
					continue
				}
				b, _ := json.Marshal(line)
				fmt.Fprintf(w, "data: %s\n\n", b)
				seenLog++
				fl.Flush()
			}
			if ev.Kind == RunEventState && isTerminalRunStatus(ev.Run.Status) {
				if seenLog == 0 {
					if logs, ok := a.Runs.Logs(target); ok {
						for _, line := range strings.Split(strings.TrimSpace(string(logs)), "\n") {
							if line == "" {
								continue
							}
							b, _ := json.Marshal(line)
							fmt.Fprintf(w, "data: %s\n\n", b)
						}
					}
				}
				fmt.Fprintf(w, "event: done\ndata: {\"rc\":%d}\n\n", ev.Run.ExitCode)
				fl.Flush()
				return
			}
		}
	}
}

func (a *App) handleLegacyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "méthode", 405)
		return
	}
	type st struct {
		ID       int    `json:"id"`
		Running  bool   `json:"running"`
		Since    int64  `json:"since"`
		Progress string `json:"progress"`
		NextRun  string `json:"nextRun"`
	}
	out := []st{}
	for _, job := range a.Jobs.List() {
		item := st{ID: job.ID}
		runs := a.Runs.List(RunFilter{JobID: job.ID, Limit: 1})
		if len(runs) > 0 && !isTerminalRunStatus(runs[0].Status) {
			item.Running = true
			if !runs[0].StartedAt.IsZero() {
				item.Since = int64(time.Since(runs[0].StartedAt).Seconds())
			}
			if logs, ok := a.Runs.Logs(runs[0].ID); ok {
				for _, line := range strings.Split(string(logs), "\n") {
					if strings.Contains(line, "%") {
						item.Progress = strings.TrimSpace(line)
					}
				}
			}
		}
		out = append(out, item)
	}
	writeJSON(w, out)
}

func (a *App) handleLegacyEngines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "méthode", http.StatusMethodNotAllowed)
		return
	}
	report := a.CapabilityReport
	if a.Capabilities != nil {
		report = a.Capabilities.Probe(r.Context())
	}
	writeJSON(w, map[string]bool{"rsync": report.ByCode(CapRsync).Status == CapabilityAvailable, "rclone": report.ByCode(CapRclone).Status == CapabilityAvailable})
}
