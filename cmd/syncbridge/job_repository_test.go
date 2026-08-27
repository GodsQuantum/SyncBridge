package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyCompatibilityJobJSONOmitsV2Fields(t *testing.T) {
	data, err := json.Marshal(Job{ID: 7, Name: "legacy", Trigger: TriggerCron})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["schemaVersion"]; exists {
		t.Fatalf("legacy writer emitted v2 schema marker: %s", data)
	}
	if _, exists := fields["action"]; exists {
		t.Fatalf("legacy writer emitted v2 action: %s", data)
	}
	if got := string(fields["trigger"]); got != `"cron"` {
		t.Fatalf("legacy trigger = %s", got)
	}
}

func fixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "jobs.json")
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestRepository(t *testing.T) *JobRepository {
	t.Helper()
	repo, err := OpenJobRepository(fixturePath(t), FileOwner{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestOpenJobRepositoryMigratesLegacyBackend(t *testing.T) {
	path := fixturePath(t)
	legacy := `{"next":2,"jobs":[{"id":1,"name":"nightly","kind":"command","command":"id","backend":"system","trigger":"cron","cron":"0 2 * * *"}]}`
	writeFixture(t, path, legacy)

	repo, err := OpenJobRepository(path, FileOwner{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil {
		t.Fatal(err)
	}
	job, ok := repo.Get(1)
	if !ok || job.SchemaVersion != 2 || job.Scheduler.Owner != SchedulerSystem || job.Action.Type != ActionCommand || !job.NeedsReview {
		t.Fatalf("unexpected migration: %#v", job)
	}

	backups, err := filepath.Glob(path + ".v1.*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("migration backup = %v, %v", backups, err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil || string(backup) != legacy {
		t.Fatalf("backup must preserve exact legacy JSON: %q, %v", backup, err)
	}
}

func TestJobRepositoryRejectsStaleRevision(t *testing.T) {
	repo := newTestRepository(t)
	job, err := repo.Create(Job{Name: "one", Action: Action{Type: ActionCommand, Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	job.Name = "two"
	if _, err := repo.Update(job.ID, job.Revision-1, job); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestJobRepositoriesSerializeMutationsAcrossInstances(t *testing.T) {
	path := fixturePath(t)
	owner := FileOwner{UID: os.Getuid(), GID: os.Getgid()}
	first, err := OpenJobRepository(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenJobRepository(path, owner)
	if err != nil {
		t.Fatal(err)
	}

	one, err := first.Create(Job{Name: "one", Action: Action{Type: ActionCommand, Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	two, err := second.Create(Job{Name: "two", Action: Action{Type: ActionCommand, Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if one.ID == two.ID {
		t.Fatalf("duplicate IDs from separate repositories: %d", one.ID)
	}

	one.Name = "one updated"
	updated, err := first.Update(one.ID, one.Revision, one)
	if err != nil {
		t.Fatal(err)
	}
	one.Name = "stale update"
	if _, err := second.Update(one.ID, one.Revision, one); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision accepted after external update: %v", err)
	}

	check, err := OpenJobRepository(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	jobs := check.List()
	if len(jobs) != 2 || jobs[0].ID != one.ID || jobs[0].Name != updated.Name || jobs[1].ID != two.ID || jobs[1].Name != "two" {
		t.Fatalf("lost mutation: %#v", jobs)
	}
}

func TestJobRepositoryReturnsIndependentSnapshots(t *testing.T) {
	repo := newTestRepository(t)
	created, err := repo.Create(Job{Name: "one", Action: Action{Type: ActionCommand, Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}

	copy := repo.List()
	copy[0].Name = "mutated"
	got, ok := repo.Get(created.ID)
	if !ok || got.Name != "one" {
		t.Fatalf("repository leaked mutable snapshot: %#v", got)
	}
}

func TestOpenJobRepositoryMarksLegacyHostJobsForReviewWithoutIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  string
	}{
		{name: "command", job: `{"id":1,"name":"cmd","kind":"command","command":"id","backend":"syncbridge","trigger":"manual"}`},
		{name: "sync", job: `{"id":1,"name":"sync","kind":"sync","source":"/src","dest":"/dst","engine":"rsync","mode":"add","backend":"syncbridge","trigger":"manual"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fixturePath(t)
			writeFixture(t, path, `{"next":2,"jobs":[`+tc.job+`]}`)
			repo, err := OpenJobRepository(path, FileOwner{UID: os.Getuid(), GID: os.Getgid()})
			if err != nil {
				t.Fatal(err)
			}
			job, ok := repo.Get(1)
			if !ok {
				t.Fatal("migrated job missing")
			}
			if !job.NeedsReview {
				t.Fatalf("legacy host job migrated runnable without identity: %#v", job)
			}
		})
	}
}
