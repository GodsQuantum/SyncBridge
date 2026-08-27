package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type executionPlanCompiler interface {
	Compile(context.Context, Job, RunRequest) (ExecutionPlan, error)
}

type PersistentScheduler struct {
	instanceID string
	compiler   executionPlanCompiler
	installer  WrapperInstaller
	runner     HostCommandRunner
	mu         sync.Mutex
}

func NewPersistentScheduler(instanceID string, compiler executionPlanCompiler, installer WrapperInstaller, runner HostCommandRunner) *PersistentScheduler {
	return &PersistentScheduler{instanceID: instanceID, compiler: compiler, installer: installer, runner: runner}
}

func persistentBase(instanceID string, jobID int) string {
	return "syncbridge-" + instanceID + "-" + strconv.Itoa(jobID)
}

func (s *PersistentScheduler) Reconcile(ctx context.Context, jobs []Job) error {
	if s == nil || s.compiler == nil || s.installer == nil {
		return errors.New("persistent scheduler is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	desired := make(map[int]Job)
	preserve := make(map[int]bool)
	var errs []error
	for _, job := range jobs {
		if job.Scheduler.Owner != SchedulerSystem {
			continue
		}
		if !job.Enabled || job.NeedsReview {
			continue
		}
		if job.Trigger != TriggerCron && job.Trigger != TriggerWatch {
			errs = append(errs, fmt.Errorf("job %d: system scheduling requires cron or watch trigger", job.ID))
			preserve[job.ID] = true
			continue
		}
		plan, err := s.compiler.Compile(ctx, job, RunRequest{JobID: job.ID, Revision: job.Revision, RunID: fmt.Sprintf("persistent-%d-%d", job.ID, job.Revision), Origin: RunOriginSystem})
		if err != nil {
			errs = append(errs, fmt.Errorf("job %d: compile persistent wrapper: %w", job.ID, err))
			preserve[job.ID] = true
			continue
		}
		wrapper, err := s.installer.Install(ctx, plan)
		if err != nil {
			errs = append(errs, fmt.Errorf("job %d: install persistent wrapper: %w", job.ID, err))
			preserve[job.ID] = true
			continue
		}
		if err := s.applyJob(ctx, job, wrapper); err != nil {
			errs = append(errs, fmt.Errorf("job %d: apply persistent scheduler: %w", job.ID, err))
			preserve[job.ID] = true
			continue
		}
		desired[job.ID] = job
	}
	if err := s.removeOrphans(ctx, desired, preserve); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *PersistentScheduler) applyJob(ctx context.Context, job Job, wrapper string) error {
	base := persistentBase(s.instanceID, job.ID)
	switch job.Trigger {
	case TriggerCron:
		if err := s.removeSystemd(ctx, base); err != nil {
			return err
		}
		content := fmt.Sprintf("%s managed job %d revision %d\nSHELL=/bin/sh\nPATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n%s root %s\n", sbMarker, job.ID, job.Revision, strings.TrimSpace(job.Cron), wrapper)
		return atomicReplaceHostFile(filepath.Join("/etc/cron.d", base), []byte(content), 0o644)
	case TriggerWatch:
		_ = s.removeCron(base)
		service := fmt.Sprintf("[Unit]\nDescription=SyncBridge persistent job %d\n\n[Service]\nType=oneshot\nExecStart=%s\nKillMode=control-group\nNoNewPrivileges=yes\n", job.ID, wrapper)
		if job.Execution.TimeoutSeconds > 0 {
			service += fmt.Sprintf("TimeoutStartSec=%ds\n", job.Execution.TimeoutSeconds)
		}
		pathUnit := fmt.Sprintf("[Unit]\nDescription=SyncBridge persistent watch %d\n\n[Path]\nPathModified=%s\nUnit=%s.service\n\n[Install]\nWantedBy=multi-user.target\n", job.ID, strings.TrimRight(job.Source, "/"), base)
		if err := atomicReplaceHostFile(filepath.Join("/etc/systemd/system", base+".service"), []byte(service), 0o644); err != nil {
			return err
		}
		if err := atomicReplaceHostFile(filepath.Join("/etc/systemd/system", base+".path"), []byte(pathUnit), 0o644); err != nil {
			return err
		}
		if s.runner == nil {
			return errors.New("host runner required to activate systemd path")
		}
		if result := s.runner.Run(ctx, "/usr/bin/env", "systemctl", "daemon-reload"); result.Err != nil {
			return fmt.Errorf("systemctl daemon-reload: %s", strings.TrimSpace(string(result.Stderr)))
		}
		if result := s.runner.Run(ctx, "/usr/bin/env", "systemctl", "enable", "--now", base+".path"); result.Err != nil {
			return fmt.Errorf("systemctl enable path: %s", strings.TrimSpace(string(result.Stderr)))
		}
		return nil
	default:
		return errors.New("unsupported persistent trigger")
	}
}

func (s *PersistentScheduler) removeCron(base string) error {
	view, err := hostFilesystemPath(filepath.Join("/etc/cron.d", base))
	if err != nil {
		return err
	}
	if err := os.Remove(view); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *PersistentScheduler) removeSystemd(ctx context.Context, base string) error {
	pathLogical := filepath.Join("/etc/systemd/system", base+".path")
	serviceLogical := filepath.Join("/etc/systemd/system", base+".service")
	pathView, _ := hostFilesystemPath(pathLogical)
	serviceView, _ := hostFilesystemPath(serviceLogical)
	existed := false
	if _, err := os.Stat(pathView); err == nil {
		existed = true
		if s.runner != nil {
			_ = s.runner.Run(ctx, "/usr/bin/env", "systemctl", "disable", "--now", base+".path")
		}
	}
	for _, view := range []string{pathView, serviceView} {
		if err := os.Remove(view); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if existed && s.runner != nil {
		if result := s.runner.Run(ctx, "/usr/bin/env", "systemctl", "daemon-reload"); result.Err != nil {
			return result.Err
		}
	}
	return nil
}

func (s *PersistentScheduler) removeJob(ctx context.Context, id int) error {
	base := persistentBase(s.instanceID, id)
	return errors.Join(s.removeCron(base), s.removeSystemd(ctx, base))
}

func (s *PersistentScheduler) removeOrphans(ctx context.Context, desired map[int]Job, preserve map[int]bool) error {
	prefix := "syncbridge-" + s.instanceID + "-"
	ids := make(map[int]bool)
	for _, logicalDir := range []string{"/etc/cron.d", "/etc/systemd/system"} {
		viewDir, err := hostFilesystemPath(logicalDir)
		if err != nil {
			continue
		}
		entries, _ := os.ReadDir(viewDir)
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			rest := strings.TrimPrefix(name, prefix)
			rest = strings.TrimSuffix(strings.TrimSuffix(rest, ".service"), ".path")
			id, err := strconv.Atoi(rest)
			if err == nil {
				ids[id] = true
			}
		}
	}
	var errs []error
	for id := range ids {
		if _, ok := desired[id]; ok || preserve[id] {
			continue
		}
		if err := s.removeJob(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("remove persistent job %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
