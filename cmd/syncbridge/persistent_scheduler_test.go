package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type persistentCompilerFake struct{ wrapper string }

func (f persistentCompilerFake) Compile(_ context.Context, job Job, req RunRequest) (ExecutionPlan, error) {
	return ExecutionPlan{JobID: job.ID, Revision: job.Revision, WrapperPath: f.wrapper}, nil
}

type persistentInstallerFake struct{ paths []string }

func (f *persistentInstallerFake) Install(_ context.Context, plan ExecutionPlan) (string, error) {
	f.paths = append(f.paths, plan.WrapperPath)
	return plan.WrapperPath, nil
}

func TestPersistentSchedulerWritesAndRemovesCronArtifact(t *testing.T) {
	root := withHostRoot(t)
	installer := &persistentInstallerFake{}
	runner := &captureHostRunner{}
	s := NewPersistentScheduler("sb-test", persistentCompilerFake{wrapper: "/var/lib/syncbridge/instances/sb-test/jobs/7/run.sh"}, installer, runner)
	job := Job{SchemaVersion: 2, ID: 7, Revision: 3, Name: "nightly", Enabled: true, Action: Action{Type: ActionCommand, Command: "true"}, Identity: Identity{Mode: IdentityFixed, User: "operator", UID: 1000, Group: "operator", GID: 1000}, Execution: ExecutionPolicy{Overlap: "skip"}, Trigger: TriggerCron, Cron: "0 2 * * *", Scheduler: SchedulerPolicy{Owner: SchedulerSystem}}
	if err := s.Reconcile(context.Background(), []Job{job}); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(root, "etc", "cron.d", "syncbridge-sb-test-7")
	data, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "0 2 * * * root /var/lib/syncbridge/instances/sb-test/jobs/7/run.sh") {
		t.Fatalf("artifact=%q", text)
	}
	if len(installer.paths) != 1 {
		t.Fatalf("installs=%#v", installer.paths)
	}
	job.Enabled = false
	job.Revision++
	if err := s.Reconcile(context.Background(), []Job{job}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("artifact still exists: %v", err)
	}
}

func TestPersistentSchedulerUsesSystemdForWatch(t *testing.T) {
	root := withHostRoot(t)
	runner := &captureHostRunner{}
	s := NewPersistentScheduler("sb-test", persistentCompilerFake{wrapper: "/var/lib/syncbridge/instances/sb-test/jobs/8/run.sh"}, &persistentInstallerFake{}, runner)
	job := Job{SchemaVersion: 2, ID: 8, Revision: 1, Name: "watch", Enabled: true, Action: Action{Type: ActionCommand, Command: "true"}, Identity: Identity{Mode: IdentityFixed, User: "operator", UID: 1000, Group: "operator", GID: 1000}, Execution: ExecutionPolicy{Overlap: "skip"}, Trigger: TriggerWatch, Source: "/srv/incoming", Scheduler: SchedulerPolicy{Owner: SchedulerSystem}}
	if err := s.Reconcile(context.Background(), []Job{job}); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "etc", "systemd", "system", "syncbridge-sb-test-8")
	service, err := os.ReadFile(base + ".service")
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.ReadFile(base + ".path")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(service), "ExecStart=/var/lib/syncbridge/instances/sb-test/jobs/8/run.sh") {
		t.Fatalf("service=%q", service)
	}
	if !strings.Contains(string(path), "PathModified=/srv/incoming") {
		t.Fatalf("path=%q", path)
	}
	var enabled bool
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), "systemctl enable --now syncbridge-sb-test-8.path") {
			enabled = true
		}
	}
	if !enabled {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestValidateV2JobRejectsSixFieldCronForSystemScheduler(t *testing.T) {
	job := Job{
		Name:      "system cron",
		Enabled:   true,
		Trigger:   TriggerCron,
		Cron:      "0 */5 * * * *",
		Scheduler: SchedulerPolicy{Owner: SchedulerSystem},
		Action:    Action{Type: ActionCommand, Command: "true"},
		Identity:  Identity{Mode: IdentityFixed, User: "operator", UID: 1000, Group: "operator", GID: 1000},
		Execution: ExecutionPolicy{Overlap: "skip"},
	}
	if err := validateV2Job(job); err == nil || !strings.Contains(err.Error(), "five fields") {
		t.Fatalf("validateV2Job error = %v, want five-field system cron rejection", err)
	}
}
