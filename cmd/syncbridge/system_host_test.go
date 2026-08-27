package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withHostRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	old := hostRootView
	hostRootView = root
	t.Cleanup(func() { hostRootView = old })
	return root
}

func TestSystemScanUsesHostRootWithoutBindMounts(t *testing.T) {
	root := withHostRoot(t)
	cronDir := filepath.Join(root, "etc", "cron.d")
	unitDir := filepath.Join(root, "etc", "systemd", "system")
	if err := os.MkdirAll(cronDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cronDir, "backup"), []byte("0 2 * * * root rsync -a /srv/a/ /srv/b/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "backup.timer"), []byte("[Timer]\nOnCalendar=daily\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewSystemService(nil)
	got := svc.Scan(context.Background())
	var cronOK, timerOK bool
	for _, it := range got.Items {
		if it.Type == "cron" && it.File == "/etc/cron.d/backup" && strings.Contains(it.Target, "rsync") {
			cronOK = true
		}
		if it.Type == "systemd-timer" && it.File == "/etc/systemd/system/backup.timer" {
			timerOK = true
		}
	}
	if !cronOK || !timerOK {
		t.Fatalf("scan = %#v", got.Items)
	}
}

func TestImportScanUsesLogicalHostPaths(t *testing.T) {
	root := withHostRoot(t)
	dir := filepath.Join(root, "srv", "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backup.sh"), []byte("#!/bin/sh\nrsync -a /srv/source/ /srv/dest/\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SB_IMPORT_PATHS", "/srv/scripts")

	svc := NewSystemService(nil)
	found, paths := svc.ScanImports(context.Background())
	if len(paths) != 1 || paths[0] != "/srv/scripts" {
		t.Fatalf("paths = %#v", paths)
	}
	if len(found) != 1 {
		t.Fatalf("found = %#v", found)
	}
	if found[0].File != "/srv/scripts/backup.sh" || found[0].Engine != "rsync" {
		t.Fatalf("found = %#v", found[0])
	}
}

func TestCronToggleWritesThroughHostRootView(t *testing.T) {
	root := withHostRoot(t)
	path := filepath.Join(root, "etc", "cron.d", "backup")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "0 2 * * * root rsync -a /srv/a/ /srv/b/\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewSystemService(nil)
	it := SysItem{Type: "cron", Name: "backup", File: "/etc/cron.d/backup", Schedule: "0 2 * * *", Target: "rsync -a /srv/a/ /srv/b/"}
	state, err := svc.Toggle(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if state != "disabled" {
		t.Fatalf("state=%q", state)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), sbOff) {
		t.Fatalf("content=%q", b)
	}
}

type captureHostRunner struct{ calls [][]string }

func (r *captureHostRunner) Run(ctx context.Context, argv ...string) CommandResult {
	r.calls = append(r.calls, append([]string(nil), argv...))
	if len(argv) >= 2 && argv[len(argv)-2] == "is-enabled" {
		return CommandResult{Stdout: []byte("disabled\n")}
	}
	return CommandResult{}
}
func (r *captureHostRunner) RunInput(ctx context.Context, input []byte, argv ...string) CommandResult {
	return r.Run(ctx, argv...)
}

func TestSystemdToggleUsesHostNamespaceRunner(t *testing.T) {
	root := withHostRoot(t)
	unit := filepath.Join(root, "etc", "systemd", "system", "backup.timer")
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Timer]\nOnCalendar=daily\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &captureHostRunner{}
	svc := NewSystemService(runner)
	state, err := svc.Toggle(context.Background(), SysItem{Type: "systemd-timer", Name: "backup.timer", File: "/etc/systemd/system/backup.timer", Schedule: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	if state != "enabled" {
		t.Fatalf("state=%q", state)
	}
	if len(runner.calls) < 2 {
		t.Fatalf("calls=%#v", runner.calls)
	}
	if got := strings.Join(runner.calls[len(runner.calls)-1], " "); !strings.Contains(got, "systemctl enable --now backup.timer") {
		t.Fatalf("last call=%q", got)
	}
}

func TestNewSystemJobDoesNotRequireReviewWhenIdentityIsExplicit(t *testing.T) {
	in := Job{Name: "persistent", Kind: "command", Command: "true", Backend: "system", Trigger: TriggerCron, Cron: "0 2 * * *", Identity: Identity{Mode: IdentityFixed, User: "operator", UID: 1000, Group: "operator", GID: 1000}}
	got, err := legacyToV2(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.NeedsReview {
		t.Fatalf("new validated system job unexpectedly needs review: %#v", got)
	}
}
