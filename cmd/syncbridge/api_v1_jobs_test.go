package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func newHTTPTestApp(t *testing.T, overlap string) (*App, *blockedExecutor) {
	t.Helper()
	repo := newTestRepository(t)
	job, err := repo.Create(Job{
		Name: "run", Enabled: true,
		Action:    Action{Type: ActionCommand, Command: "true"},
		Identity:  Identity{Mode: IdentityFixed, User: "user", UID: 1000, Group: "user", GID: 1000},
		Execution: ExecutionPolicy{Overlap: overlap}, Trigger: TriggerManual,
		Scheduler: SchedulerPolicy{Owner: SchedulerSyncBridge},
	})
	if err != nil || job.ID != 1 {
		t.Fatalf("create = %#v, %v", job, err)
	}
	executor := &blockedExecutor{}
	runs := NewRunService(repo, testCompiler{}, testInstaller{}, executor, testLimits())
	return &App{Jobs: repo, Runs: runs}, executor
}

func requestJSON(t *testing.T, client *http.Client, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeResponseJSON(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatal(err)
	}
}

func TestStartRunReturnsReservedRun(t *testing.T) {
	app, _ := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	resp := requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/v1/jobs/1/runs", `{"revision":1,"dryRun":false}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var body struct {
		Run RunSnapshot `json:"run"`
	}
	decodeResponseJSON(t, resp, &body)
	if body.Run.ID == "" || body.Run.Status != RunQueued {
		t.Fatalf("run=%#v", body.Run)
	}
}

func TestConcurrentRunRequestsReturnOneConflict(t *testing.T) {
	app, _ := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/v1/jobs/1/runs", `{"revision":1}`, nil)
			statuses <- r.StatusCode
			r.Body.Close()
		}()
	}
	wg.Wait()
	close(statuses)
	got := []int{}
	for s := range statuses {
		got = append(got, s)
	}
	sort.Ints(got)
	want := []int{http.StatusAccepted, http.StatusConflict}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("statuses=%v", got)
	}
}

func TestV1JobCRUDUsesETagRevision(t *testing.T) {
	app, _ := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	create := `{"name":"two","enabled":true,"action":{"type":"command","command":"id"},"identity":{"mode":"fixed","user":"u","uid":1000,"group":"g","gid":1000},"execution":{"overlap":"skip"},"trigger":"manual","scheduler":{"owner":"syncbridge"}}`
	resp := requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/v1/jobs", create, nil)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create=%d %s", resp.StatusCode, b)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	var created Job
	decodeResponseJSON(t, resp, &created)
	if created.ID != 2 || created.Revision != 1 {
		t.Fatalf("created=%#v", created)
	}

	created.Name = "updated"
	payload, _ := json.Marshal(created)
	resp = requestJSON(t, s.Client(), http.MethodPut, s.URL+"/api/v1/jobs/2", string(payload), map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("update=%d %s", resp.StatusCode, b)
	}
	etag2 := resp.Header.Get("ETag")
	if etag2 == etag || etag2 == "" {
		t.Fatalf("etag2=%q", etag2)
	}
	var updated Job
	decodeResponseJSON(t, resp, &updated)
	if updated.Revision != 2 || updated.Name != "updated" {
		t.Fatalf("updated=%#v", updated)
	}

	resp = requestJSON(t, s.Client(), http.MethodDelete, s.URL+"/api/v1/jobs/2", "", map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stale delete=%d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	resp = requestJSON(t, s.Client(), http.MethodDelete, s.URL+"/api/v1/jobs/2", "", map[string]string{"If-Match": etag2})
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete=%d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestV1RejectsUnknownJSONFieldsAndWrongContentType(t *testing.T) {
	app, _ := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	resp := requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/v1/jobs/1/runs", `{"revision":1,"unknown":true}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/v1/jobs/1/runs", bytes.NewBufferString(`{"revision":1}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestV1RunLookupListAndStop(t *testing.T) {
	app, executor := newHTTPTestApp(t, "skip")
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	resp := requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/v1/jobs/1/runs", `{"revision":1}`, nil)
	var body struct {
		Run RunSnapshot `json:"run"`
	}
	decodeResponseJSON(t, resp, &body)
	waitForStarts(t, executor, 1)
	resp = requestJSON(t, s.Client(), http.MethodGet, s.URL+"/api/v1/runs/"+body.Run.ID, "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = requestJSON(t, s.Client(), http.MethodGet, s.URL+"/api/v1/jobs/1/runs", "", nil)
	var list []RunSnapshot
	decodeResponseJSON(t, resp, &list)
	if len(list) != 1 || list[0].ID != body.Run.ID {
		t.Fatalf("list=%#v", list)
	}
	resp = requestJSON(t, s.Client(), http.MethodPost, s.URL+"/api/v1/runs/"+body.Run.ID+"/stop", `{}`, nil)
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stop=%d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestV1EventsReplaysAfterLastEventID(t *testing.T) {
	app, _ := newHTTPTestApp(t, "skip")
	first, err := app.Runs.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	sub := app.Runs.Subscribe(0)
	var firstEvent RunEvent
	select {
	case firstEvent = <-sub.Events:
		sub.Close()
	default:
		sub.Close()
		t.Fatal("missing retained event")
	}
	if firstEvent.ID == 0 || first.ID == "" {
		t.Fatalf("event=%#v run=%#v", firstEvent, first)
	}
	s := httptest.NewServer(app.Handler())
	defer s.Close()
	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/v1/events", nil)
	req.Header.Set("Last-Event-ID", strconv.FormatUint(firstEvent.ID-1, 10))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	cancel()
	resp.Body.Close()
	if !strings.Contains(string(buf[:n]), "id: "+strconv.FormatUint(firstEvent.ID, 10)) {
		t.Fatalf("sse=%q", buf[:n])
	}
}
