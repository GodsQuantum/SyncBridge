package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

func (a *App) handleV1Jobs(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.Jobs == nil {
		writeAPIError(w, 500, "not_configured", "job repository is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSONStatus(w, http.StatusOK, a.Jobs.List())
	case http.MethodPost:
		var job Job
		if err := decodeStrictJSON(w, r, &job); err != nil {
			handleDecodeError(w, err)
			return
		}
		normalizeV2Job(&job)
		if err := validateV2Job(job); err != nil {
			writeAPIError(w, 400, "invalid_job", err.Error())
			return
		}
		created, err := a.Jobs.Create(job)
		if err != nil {
			mapServiceError(w, err)
			return
		}
		w.Header().Set("ETag", revisionETag(created.Revision))
		writeJSONStatus(w, http.StatusCreated, created)
		a.reconcileTriggers()
	default:
		requireMethod(w, r, http.MethodGet, http.MethodPost)
	}
}

func (a *App) handleV1Job(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeAPIError(w, 404, "not_found", "resource not found")
		return
	}
	id, err := parsePositiveID(parts[0])
	if err != nil {
		writeAPIError(w, 400, "invalid_id", "invalid job id")
		return
	}
	if len(parts) == 2 && parts[1] == "runs" {
		a.handleV1JobRuns(w, r, id)
		return
	}
	if len(parts) != 1 {
		writeAPIError(w, 404, "not_found", "resource not found")
		return
	}
	if a == nil || a.Jobs == nil {
		writeAPIError(w, 500, "not_configured", "job repository is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		job, ok := a.Jobs.Get(id)
		if !ok {
			mapServiceError(w, ErrJobNotFound)
			return
		}
		w.Header().Set("ETag", revisionETag(job.Revision))
		writeJSONStatus(w, 200, job)
	case http.MethodPut:
		revision, err := parseIfMatch(r)
		if err != nil {
			writeAPIError(w, 428, "precondition_required", err.Error())
			return
		}
		var job Job
		if err := decodeStrictJSON(w, r, &job); err != nil {
			handleDecodeError(w, err)
			return
		}
		normalizeV2Job(&job)
		if err := validateV2Job(job); err != nil {
			writeAPIError(w, 400, "invalid_job", err.Error())
			return
		}
		updated, err := a.Jobs.Update(id, revision, job)
		if err != nil {
			mapServiceError(w, err)
			return
		}
		w.Header().Set("ETag", revisionETag(updated.Revision))
		writeJSONStatus(w, 200, updated)
		a.reconcileTriggers()
	case http.MethodDelete:
		revision, err := parseIfMatch(r)
		if err != nil {
			writeAPIError(w, 428, "precondition_required", err.Error())
			return
		}
		if a.jobHasActiveRun(id) {
			writeAPIError(w, 409, "active_run", "stop active run before deleting job")
			return
		}
		if err := a.Jobs.Delete(id, revision); err != nil {
			mapServiceError(w, err)
			return
		}
		a.reconcileTriggers()
		writeJSONStatus(w, http.StatusNoContent, nil)
	default:
		requireMethod(w, r, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (a *App) handleV1JobRuns(w http.ResponseWriter, r *http.Request, id int) {
	if a == nil || a.Runs == nil {
		writeAPIError(w, 500, "not_configured", "run service is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if a.Jobs != nil {
			if _, ok := a.Jobs.Get(id); !ok {
				mapServiceError(w, ErrJobNotFound)
				return
			}
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, e := strconv.Atoi(raw); e == nil && n > 0 && n <= 1000 {
				limit = n
			} else {
				writeAPIError(w, 400, "invalid_limit", "limit must be between 1 and 1000")
				return
			}
		}
		writeJSONStatus(w, 200, a.Runs.List(RunFilter{JobID: id, Limit: limit}))
	case http.MethodPost:
		var input struct {
			Revision uint64 `json:"revision"`
			DryRun   bool   `json:"dryRun"`
		}
		if err := decodeStrictJSON(w, r, &input); err != nil {
			handleDecodeError(w, err)
			return
		}
		if input.Revision == 0 {
			writeAPIError(w, 400, "invalid_revision", "revision must be positive")
			return
		}
		run, err := a.Runs.Start(r.Context(), StartRunInput{JobID: id, Revision: input.Revision, Origin: RunOriginManual, DryRun: input.DryRun})
		if err != nil {
			if errors.Is(err, ErrOverlap) {
				writeJSONStatus(w, http.StatusConflict, map[string]any{"run": run, "error": apiError{Code: "conflict", Message: err.Error()}})
				return
			}
			mapServiceError(w, err)
			return
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"run": run})
	default:
		requireMethod(w, r, http.MethodGet, http.MethodPost)
	}
}

func normalizeV2Job(job *Job) {
	job.SchemaVersion = 2
	job.ID = 0
	job.Revision = 0
	if job.Execution.Overlap == "" {
		job.Execution.Overlap = "skip"
	}
	if job.Scheduler.Owner == "" {
		job.Scheduler.Owner = SchedulerSyncBridge
	}
	if job.Trigger == "" {
		job.Trigger = TriggerManual
	}
}

func validateV2Job(job Job) error {
	if strings.TrimSpace(job.Name) == "" {
		return errors.New("name is required")
	}
	if job.Trigger != TriggerManual && job.Trigger != TriggerCron && job.Trigger != TriggerWatch {
		return errors.New("trigger must be manual, cron, or watch")
	}
	if job.Scheduler.Owner != SchedulerSyncBridge && job.Scheduler.Owner != SchedulerSystem {
		return errors.New("scheduler owner must be syncbridge or system")
	}
	if job.Scheduler.Owner == SchedulerSystem && job.Trigger == TriggerManual {
		return errors.New("system scheduling requires cron or watch trigger")
	}
	if job.Trigger == TriggerCron {
		if _, err := normalizeCronSpec(job.Cron); err != nil {
			return err
		}
		if job.Scheduler.Owner == SchedulerSystem && len(strings.Fields(job.Cron)) != 5 {
			return errors.New("system cron expressions must have exactly five fields")
		}
	}
	if job.Trigger == TriggerWatch && strings.TrimSpace(job.Source) == "" {
		return errors.New("watch trigger requires a source directory")
	}
	if job.Execution.Overlap != "skip" && job.Execution.Overlap != "queue-latest" {
		return errors.New("overlap must be skip or queue-latest")
	}
	if job.Execution.TimeoutSeconds < 0 || job.Execution.StopGraceSeconds < 0 || job.Execution.Umask > 0o777 {
		return errors.New("invalid execution bounds")
	}
	for _, entry := range job.Execution.Environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !environmentNamePattern.MatchString(name) || strings.IndexByte(value, 0) >= 0 {
			return errors.New("invalid environment")
		}
	}
	switch job.Action.Type {
	case ActionScript:
		if !filepath.IsAbs(job.Action.ScriptPath) || filepath.Clean(job.Action.ScriptPath) != job.Action.ScriptPath {
			return errors.New("script path must be absolute and canonical")
		}
		if job.Identity.Mode != IdentityScriptOwner && job.Identity.Mode != IdentityFixed {
			return errors.New("script identity must be script-owner or fixed")
		}
	case ActionCommand:
		if strings.TrimSpace(job.Action.Command) == "" || strings.IndexByte(job.Action.Command, 0) >= 0 {
			return errors.New("command is required")
		}
		if job.Identity.Mode != IdentityFixed {
			return errors.New("command actions require a fixed identity")
		}
	case ActionSync:
		s := job.Action.Sync
		if err := validateSyncActionOptions(s); err != nil {
			return err
		}
		if !filepath.IsAbs(s.Source) || !filepath.IsAbs(s.Dest) || filepath.Clean(s.Source) != s.Source || filepath.Clean(s.Dest) != s.Dest || s.Source == s.Dest {
			return errors.New("sync paths must be distinct absolute canonical paths")
		}
		if pathWithin(s.Source, s.Dest) || pathWithin(s.Dest, s.Source) {
			return errors.New("sync paths must not contain one another")
		}
		if job.Identity.Mode != IdentityFixed {
			return errors.New("sync actions require a fixed identity")
		}
	default:
		return errors.New("unknown action type")
	}
	if job.Identity.Mode == IdentityFixed {
		if !accountNamePattern.MatchString(job.Identity.User) || !accountNamePattern.MatchString(job.Identity.Group) || job.Identity.UID < 0 || job.Identity.GID < 0 {
			return errors.New("fixed identity is incomplete or invalid")
		}
	}
	return nil
}

func (a *App) jobHasActiveRun(id int) bool {
	if a == nil || a.Runs == nil {
		return false
	}
	for _, run := range a.Runs.List(RunFilter{JobID: id}) {
		if !isTerminalRunStatus(run.Status) {
			return true
		}
	}
	return false
}

func (a *App) reconcileTriggers() error {
	if a == nil || a.Jobs == nil {
		return nil
	}
	jobs := a.Jobs.List()
	var errs []error
	if a.Scheduler != nil {
		errs = append(errs, a.Scheduler.Reconcile(jobs))
	}
	if a.Watches != nil {
		errs = append(errs, a.Watches.Reconcile(jobs))
	}
	if a.Persistent != nil {
		errs = append(errs, a.Persistent.Reconcile(context.Background(), jobs))
	}
	return errors.Join(errs...)
}
