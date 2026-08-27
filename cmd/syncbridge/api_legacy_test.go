package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLegacyCompatibilityJobsUseRepositoryAndExposeIdentity(t *testing.T) {
	app, _ := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	resp := requestJSON(t, s.Client(), http.MethodGet, s.URL+"/api/jobs", "", nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("get=%d %s", resp.StatusCode, b)
	}
	var jobs []map[string]any
	decodeResponseJSON(t, resp, &jobs)
	if len(jobs) != 1 || jobs[0]["kind"] != "command" || jobs[0]["revision"].(float64) != 1 {
		t.Fatalf("jobs=%#v", jobs)
	}
	identity, ok := jobs[0]["identity"].(map[string]any)
	if !ok || identity["user"] != "user" {
		t.Fatalf("identity=%#v", jobs[0]["identity"])
	}
}

func TestLegacyCompatibilityMutationsUseRepositoryRevisions(t *testing.T) {
	app, _ := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	resp := requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/jobs/1/clone", `{}`, nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("clone=%d %s", resp.StatusCode, b)
	}
	var clone map[string]any
	decodeResponseJSON(t, resp, &clone)
	if clone["id"].(float64) != 2 || clone["disabled"] != true {
		t.Fatalf("clone=%#v", clone)
	}
	job, ok := app.Jobs.Get(2)
	if !ok || job.Revision != 1 || job.Enabled {
		t.Fatalf("repo clone=%#v", job)
	}

	resp = requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/jobs/2/toggle", `{}`, nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("toggle=%d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	job, _ = app.Jobs.Get(2)
	if job.Revision != 2 || !job.Enabled {
		t.Fatalf("toggled=%#v", job)
	}

	resp = requestJSON(t, s.Client(), http.MethodDelete, s.URL+"/api/jobs/2", "", nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete=%d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	if _, ok := app.Jobs.Get(2); ok {
		t.Fatal("legacy delete did not delete repository job")
	}
}

func TestLegacyCompatibilityCreateRequiresExplicitHostIdentity(t *testing.T) {
	app, _ := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	without := `{"name":"cmd","kind":"command","command":"id","backend":"syncbridge","trigger":"manual"}`
	resp := requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/jobs", without, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("without identity=%d", resp.StatusCode)
	}
	resp.Body.Close()
	with := `{"name":"cmd","kind":"command","command":"id","backend":"syncbridge","trigger":"manual","identity":{"mode":"fixed","user":"operator","uid":1000,"group":"operator","gid":1000}}`
	resp = requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/jobs", with, nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("with identity=%d %s", resp.StatusCode, b)
	}
	var body map[string]any
	decodeResponseJSON(t, resp, &body)
	if body["revision"].(float64) != 1 {
		t.Fatalf("body=%#v", body)
	}
	job, _ := app.Jobs.Get(2)
	if job.Action.Type != ActionCommand || job.Identity.User != "operator" || job.NeedsReview {
		t.Fatalf("job=%#v", job)
	}
}

func TestLegacyRunAndKillUseRunService(t *testing.T) {
	app, executor := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	resp := requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/jobs/1/run?dry=0", `{}`, nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("run=%d %s", resp.StatusCode, b)
	}
	var started struct {
		RunID string `json:"runId"`
	}
	decodeResponseJSON(t, resp, &started)
	if started.RunID == "" {
		t.Fatal("missing opaque run id")
	}
	waitForStarts(t, executor, 1)
	resp = requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/jobs/1/kill", `{}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("kill=%d", resp.StatusCode)
	}
	var killed map[string]any
	decodeResponseJSON(t, resp, &killed)
	if killed["killed"] != true {
		t.Fatalf("kill=%#v", killed)
	}
}

func TestLegacyJobJSONCarriesRevisionAndIdentity(t *testing.T) {
	view := legacyJobView(Job{SchemaVersion: 2, ID: 7, Revision: 3, Name: "x", Enabled: true, Action: Action{Type: ActionCommand, Command: "id"}, Identity: Identity{Mode: IdentityFixed, User: "u", UID: 1, Group: "g", GID: 2}, Execution: ExecutionPolicy{Overlap: "skip"}, Trigger: TriggerManual, Scheduler: SchedulerPolicy{Owner: SchedulerSyncBridge}})
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body["revision"].(float64) != 3 || body["kind"] != "command" {
		t.Fatalf("body=%s", data)
	}
	if _, ok := body["identity"].(map[string]any); !ok {
		t.Fatalf("missing identity: %s", data)
	}
}

func TestLegacyAdvancedSyncOptionsRoundTripThroughV2(t *testing.T) {
	legacy := Job{
		Name: "advanced sync", Kind: "sync", Engine: "rsync", Source: "/srv/source", Dest: "/srv/destination", Mode: "mirror",
		Compare: "checksum", Bwlimit: "30M", Backup: true, BackupKeep: 4, MaxDel: 50, SkipNew: true, SysBackup: true, Exclude: "*.tmp,cache/**",
		Backend: "syncbridge", Trigger: TriggerManual,
		Identity: Identity{Mode: IdentityFixed, User: "operator", UID: 1000, Group: "operator", GID: 1000},
	}
	job, err := legacyToV2(legacy)
	if err != nil {
		t.Fatal(err)
	}
	sync := job.Action.Sync
	if sync.Compare != legacy.Compare || sync.Bwlimit != legacy.Bwlimit || !sync.Backup || sync.BackupKeep != legacy.BackupKeep || sync.MaxDel != legacy.MaxDel || !sync.SkipNew || !sync.SysBackup || sync.Exclude != legacy.Exclude {
		t.Fatalf("advanced sync options lost during legacy -> v2 conversion: %#v", sync)
	}
	view := legacyJobView(job)
	if view.Compare != legacy.Compare || view.Bwlimit != legacy.Bwlimit || !view.Backup || view.BackupKeep != legacy.BackupKeep || view.MaxDel != legacy.MaxDel || !view.SkipNew || !view.SysBackup || view.Exclude != legacy.Exclude {
		t.Fatalf("advanced sync options lost during v2 -> legacy conversion: %#v", view)
	}
}
