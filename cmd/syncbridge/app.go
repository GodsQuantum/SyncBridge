package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type App struct {
	Jobs             *JobRepository
	Runs             *RunService
	Scheduler        *Scheduler
	Watches          *WatchManager
	Capabilities     *CapabilityService
	CapabilityReport CapabilityReport
	System           *SystemService
	Persistent       *PersistentScheduler
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/jobs", a.handleV1Jobs)
	mux.HandleFunc("/api/v1/jobs/", a.handleV1Job)
	mux.HandleFunc("/api/v1/runs/", a.handleV1Run)
	mux.HandleFunc("/api/v1/events", a.handleV1Events)
	mux.HandleFunc("/api/v1/capabilities", a.handleV1Capabilities)
	mux.HandleFunc("/api/jobs", a.handleLegacyJobs)
	mux.HandleFunc("/api/jobs/", a.handleLegacyJobByID)
	mux.HandleFunc("/api/status", a.handleLegacyStatus)
	mux.HandleFunc("/api/browse", apiBrowse)
	mux.HandleFunc("/api/engines", a.handleLegacyEngines)
	mux.HandleFunc("/api/auth/status", apiAuthStatus)
	mux.HandleFunc("/api/auth/register", apiAuthRegister)
	mux.HandleFunc("/api/auth/login", apiAuthLogin)
	mux.HandleFunc("/api/auth/logout", apiAuthLogout)
	mux.HandleFunc("/api/remotes", apiRemotes)
	mux.HandleFunc("/api/remotes/", apiRemoteByID)
	mux.HandleFunc("/api/remote/", apiRemoteProxy)
	mux.HandleFunc("/api/settings", apiSettings)
	mux.HandleFunc("/api/import/scan", a.handleImportScan)
	mux.HandleFunc("/api/system/scan", a.handleSystemScan)
	mux.HandleFunc("/api/system/toggle", a.handleSystemToggle)
	mux.HandleFunc("/api/system/delete", a.handleSystemDelete)
	mux.HandleFunc("/api/system/trash", a.handleSystemTrash)
	mux.HandleFunc("/api/system/restore", a.handleSystemRestore)
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	return mux
}

func (a *App) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if a.Scheduler != nil {
		a.Scheduler.Close()
	}
	if a.Watches != nil {
		a.Watches.Close()
	}
	if a.Runs == nil {
		return nil
	}
	var errs []error
	for _, run := range a.Runs.List(RunFilter{}) {
		if !isTerminalRunStatus(run.Status) {
			if err := a.Runs.Stop(ctx, run.ID); err != nil && !errors.Is(err, ErrRunNotActive) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (a *App) handleV1Run(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.Runs == nil {
		writeAPIError(w, 500, "not_configured", "run service is unavailable")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/runs/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeAPIError(w, 404, "not_found", "resource not found")
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "stop" {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var body map[string]any
		if err := decodeStrictJSON(w, r, &body); err != nil {
			handleDecodeError(w, err)
			return
		}
		if len(body) != 0 {
			writeAPIError(w, 400, "invalid_json", "stop body must be empty JSON object")
			return
		}
		if err := a.Runs.Stop(r.Context(), id); err != nil {
			mapServiceError(w, err)
			return
		}
		run, _ := a.Runs.Get(id)
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"run": run})
		return
	}
	if len(parts) != 1 {
		writeAPIError(w, 404, "not_found", "resource not found")
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	run, ok := a.Runs.Get(id)
	if !ok {
		mapServiceError(w, ErrRunNotFound)
		return
	}
	writeJSONStatus(w, 200, run)
}

func (a *App) handleV1Events(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if a == nil || a.Runs == nil {
		writeAPIError(w, 500, "not_configured", "run service is unavailable")
		return
	}
	after := uint64(0)
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeAPIError(w, 400, "invalid_event_id", "Last-Event-ID must be an unsigned integer")
			return
		}
		after = n
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, 500, "stream_unsupported", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sub := a.Runs.Subscribe(after)
	defer sub.Close()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-sub.Events:
			if !ok {
				return
			}
			data, err := jsonMarshal(event)
			if err != nil {
				return
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Kind, data)
			flusher.Flush()
		}
	}
}

func (a *App) handleV1Capabilities(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if a == nil {
		writeAPIError(w, http.StatusInternalServerError, "not_configured", "capability service is unavailable")
		return
	}
	report := a.CapabilityReport
	if a.Capabilities != nil {
		report = a.Capabilities.Probe(r.Context())
	}
	writeJSONStatus(w, 200, report)
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func parseFileOwner(uidText, gidText string) (FileOwner, error) {
	uid, err := strconv.Atoi(strings.TrimSpace(uidText))
	if err != nil || uid < 0 {
		return FileOwner{}, errors.New("SB_UID must be a non-negative numeric ID")
	}
	gid, err := strconv.Atoi(strings.TrimSpace(gidText))
	if err != nil || gid < 0 {
		return FileOwner{}, errors.New("SB_GID must be a non-negative numeric ID")
	}
	return FileOwner{UID: uid, GID: gid}, nil
}

func loadOrCreateInstanceID(dir string, owner FileOwner) (string, error) {
	path := filepath.Join(dir, "instance-id")
	if data, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(data))
		if !instanceIDPattern.MatchString(id) {
			return "", errors.New("persisted instance ID is invalid")
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read instance ID: %w", err)
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate instance ID: %w", err)
	}
	id := "sb-" + hex.EncodeToString(buf)
	if err := AtomicWriteFile(path, []byte(id+"\n"), 0o600, owner); err != nil {
		return "", fmt.Errorf("persist instance ID: %w", err)
	}
	return id, nil
}

func NewProductionApp(ctx context.Context, configDir string, owner FileOwner) (*App, error) {
	if ctx == nil {
		return nil, errors.New("application context is nil")
	}
	if configDir == "" {
		return nil, errors.New("config directory is empty")
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chown(configDir, owner.UID, owner.GID); err != nil {
		return nil, fmt.Errorf("chown config directory: %w", err)
	}
	instanceID, err := loadOrCreateInstanceID(configDir, owner)
	if err != nil {
		return nil, err
	}
	repo, err := OpenJobRepository(filepath.Join(configDir, "jobs.json"), owner)
	if err != nil {
		return nil, err
	}
	runner := NewHostCommandRunner()
	caps := NewCapabilityService(runner)
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	report := caps.Probe(probeCtx)
	cancel()
	tools, _ := report.HostTools()
	compiler := NewPlanCompiler(instanceID, report, NewRunnerHostInspector(runner, tools))
	store := NewWrapperStore(runner, NewWrapperRenderer())
	runs := NewRunService(repo, compiler, store, NewHostExecutor(), LogLimits{MaxBytes: 4 << 20, MaxLineBytes: MaxExecutionLogLineBytes, MaxRuns: 200, MaxAge: 24 * time.Hour})
	scheduler := NewScheduler(ctx, runs)
	watches := NewWatchManager(ctx, runs)
	app := &App{Jobs: repo, Runs: runs, Scheduler: scheduler, Watches: watches, Capabilities: caps, CapabilityReport: report, System: NewSystemService(runner), Persistent: NewPersistentScheduler(instanceID, compiler, store, runner)}
	if err := app.reconcileTriggers(); err != nil {
		scheduler.Close()
		watches.Close()
		return nil, fmt.Errorf("reconcile triggers: %w", err)
	}
	return app, nil
}
