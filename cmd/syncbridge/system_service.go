package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sbMarker = "# syncbridge"
	sbOff    = "#SB-OFF# "
)

type SysItem struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Target   string `json:"target"`
	File     string `json:"file"`
	Managed  bool   `json:"managed"`
	Class    string `json:"class"`
	Disabled bool   `json:"disabled"`
}

type SystemInventory struct {
	Items        []SysItem `json:"items"`
	CronPaths    []string  `json:"cronPaths"`
	SystemdPaths []string  `json:"systemdPaths"`
}

type TrashEntry struct {
	TS         string `json:"ts"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	File       string `json:"file"`
	Schedule   string `json:"schedule"`
	Target     string `json:"target"`
	Line       string `json:"line"`
	Unit       string `json:"unit"`
	Restorable bool   `json:"restorable"`
}

type SystemService struct {
	runner HostCommandRunner
	mu     sync.Mutex
}

func NewSystemService(runner HostCommandRunner) *SystemService { return &SystemService{runner: runner} }

func systemCronPaths() []string {
	return []string{"/etc/crontab", "/etc/cron.d", "/var/spool/cron/crontabs"}
}
func systemdPaths() []string { return []string{"/etc/systemd/system"} }

func (s *SystemService) Scan(ctx context.Context) SystemInventory {
	items := make([]SysItem, 0)
	for _, logical := range systemCronPaths() {
		if ctx.Err() != nil {
			break
		}
		items = append(items, scanCronPath(logical)...)
	}
	for _, logical := range systemdPaths() {
		if ctx.Err() != nil {
			break
		}
		items = append(items, scanSystemdPath(logical)...)
	}
	items = append(items, scanInotifyProcesses(ctx)...)
	for i := range items {
		items[i].Class = classifySys(items[i])
	}
	return SystemInventory{Items: items, CronPaths: systemCronPaths(), SystemdPaths: systemdPaths()}
}

func scanCronPath(logical string) []SysItem {
	view, err := hostFilesystemPath(logical)
	if err != nil {
		return nil
	}
	info, err := os.Stat(view)
	if err != nil {
		return nil
	}
	files := []string{view}
	if info.IsDir() {
		entries, _ := os.ReadDir(view)
		files = files[:0]
		for _, entry := range entries {
			if !entry.IsDir() {
				files = append(files, filepath.Join(view, entry.Name()))
			}
		}
	}
	var out []SysItem
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		logicalFile := logical
		if info.IsDir() {
			logicalFile = filepath.Join(logical, filepath.Base(file))
		}
		managed := strings.Contains(string(data), sbMarker)
		for _, line := range strings.Split(string(data), "\n") {
			raw := strings.TrimSpace(line)
			disabled := false
			if strings.HasPrefix(raw, sbOff) {
				disabled = true
				raw = strings.TrimSpace(strings.TrimPrefix(raw, sbOff))
			}
			if item := parseCronLine(raw, logicalFile, managed); item != nil {
				item.Disabled = disabled
				out = append(out, *item)
			}
		}
	}
	return out
}

func parseCronLine(line, file string, managed bool) *SysItem {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return nil
	}
	for i := 0; i < 5; i++ {
		if !isCronField(fields[i]) {
			return nil
		}
	}
	schedule := strings.Join(fields[:5], " ")
	rest := fields[5:]
	if (strings.Contains(file, "/cron.d/") || file == "/etc/crontab") && len(rest) > 1 {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return nil
	}
	return &SysItem{Type: "cron", Name: filepath.Base(file), Schedule: schedule, Target: strings.Join(rest, " "), File: file, Managed: managed}
}

func scanSystemdPath(logical string) []SysItem {
	view, err := hostFilesystemPath(logical)
	if err != nil {
		return nil
	}
	var out []SysItem
	for _, ext := range []string{"*.service", "*.timer", "*.path"} {
		files, _ := filepath.Glob(filepath.Join(view, ext))
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			content := string(data)
			logicalFile := filepath.Join(logical, filepath.Base(file))
			item := SysItem{Name: filepath.Base(file), File: logicalFile, Managed: strings.Contains(content, sbMarker)}
			switch {
			case strings.HasSuffix(file, ".service"):
				item.Type = "systemd-service"
				item.Target = iniVal(content, "ExecStart")
			case strings.HasSuffix(file, ".timer"):
				item.Type = "systemd-timer"
				item.Schedule = firstNonEmpty(iniVal(content, "OnCalendar"), iniVal(content, "OnUnitActiveSec"))
			case strings.HasSuffix(file, ".path"):
				item.Type = "systemd-path"
				item.Schedule = firstNonEmpty(iniVal(content, "PathModified"), iniVal(content, "PathChanged"), iniVal(content, "PathExists"))
			}
			out = append(out, item)
		}
	}
	return out
}

func iniVal(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+"="))
		}
	}
	return ""
}
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func scanInotifyProcesses(ctx context.Context) []SysItem {
	files, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	var out []SysItem
	for _, file := range files {
		if ctx.Err() != nil {
			break
		}
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if len(args) == 0 || !strings.Contains(filepath.Base(args[0]), "inotifywait") {
			continue
		}
		pid := filepath.Base(filepath.Dir(file))
		out = append(out, SysItem{Type: "inotify-proc", Name: "pid " + pid, Schedule: "inotifywait", Target: strings.Join(args, " "), File: "/proc/" + pid + "/cmdline"})
	}
	return out
}

func classifySys(item SysItem) string {
	if item.Managed {
		return "managed"
	}
	text := strings.ToLower(item.Target + " " + item.Schedule + " " + item.File + " " + item.Name)
	for _, hint := range []string{"run-parts", "/usr/lib/", "/lib/systemd", "systemd-tmpfiles", "logrotate", "updatedb", "fstrim", "sysstat"} {
		if strings.Contains(text, hint) {
			return "system"
		}
	}
	for _, hint := range []string{".sh", ".py", ".bash", "/home/", "/mnt/", "/opt/", "/srv/", "/root/", "inotifywait", "rsync", "rclone"} {
		if strings.Contains(text, hint) {
			return "custom"
		}
	}
	return "unknown"
}

func sameSysItem(a, b SysItem) bool {
	return a.Type == b.Type && a.Name == b.Name && a.File == b.File && a.Schedule == b.Schedule && a.Target == b.Target
}

func (s *SystemService) currentItem(ctx context.Context, requested SysItem) (SysItem, error) {
	for _, current := range s.Scan(ctx).Items {
		if sameSysItem(current, requested) {
			return current, nil
		}
	}
	return SysItem{}, errors.New("system item is stale or no longer exists; rescan before changing it")
}

func (s *SystemService) Toggle(ctx context.Context, requested SysItem) (string, error) {
	item, err := s.currentItem(ctx, requested)
	if err != nil {
		return "", err
	}
	switch item.Type {
	case "cron":
		return toggleCron(item)
	case "systemd-service", "systemd-timer", "systemd-path":
		return s.toggleSystemd(ctx, item.Name)
	case "inotify-proc":
		return killInotify(item)
	default:
		return "", fmt.Errorf("unsupported system item type %q", item.Type)
	}
}

func logicalToView(path string) (string, error) { return hostFilesystemPath(path) }

func atomicReplaceHostFile(logical string, content []byte, fallbackMode os.FileMode) error {
	view, err := logicalToView(logical)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(view)
	mode := fallbackMode
	uid, gid := -1, -1
	if statErr == nil {
		mode = info.Mode().Perm()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	if err := os.MkdirAll(filepath.Dir(view), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(view), ".syncbridge-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if uid >= 0 {
		_ = tmp.Chown(uid, gid)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, view)
}

func toggleCron(item SysItem) (string, error) {
	view, err := logicalToView(item.File)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(view)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		body := trimmed
		off := false
		if strings.HasPrefix(trimmed, sbOff) {
			off = true
			body = strings.TrimSpace(strings.TrimPrefix(trimmed, sbOff))
		}
		parsed := parseCronLine(body, item.File, false)
		if parsed == nil || parsed.Schedule != item.Schedule || parsed.Target != item.Target {
			continue
		}
		if off {
			lines[i] = body
		} else {
			lines[i] = sbOff + line
		}
		if err := atomicReplaceHostFile(item.File, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			return "", err
		}
		if off {
			return "enabled", nil
		}
		return "disabled", nil
	}
	return "", errors.New("cron entry no longer matches; rescan before changing it")
}

func (s *SystemService) toggleSystemd(ctx context.Context, name string) (string, error) {
	if s.runner == nil {
		return "", errors.New("host command runner is unavailable")
	}
	state := s.runner.Run(ctx, "/usr/bin/env", "systemctl", "is-enabled", name)
	if strings.TrimSpace(string(state.Stdout)) == "enabled" {
		res := s.runner.Run(ctx, "/usr/bin/env", "systemctl", "disable", "--now", name)
		if res.Err != nil {
			return "", fmt.Errorf("disable systemd unit: %s", strings.TrimSpace(string(res.Stderr)))
		}
		return "disabled", nil
	}
	res := s.runner.Run(ctx, "/usr/bin/env", "systemctl", "enable", "--now", name)
	if res.Err != nil {
		return "", fmt.Errorf("enable systemd unit: %s", strings.TrimSpace(string(res.Stderr)))
	}
	return "enabled", nil
}

func killInotify(item SysItem) (string, error) {
	parts := strings.Split(item.File, "/")
	if len(parts) < 4 || parts[1] != "proc" {
		return "", errors.New("invalid process identity")
	}
	pid, err := strconv.Atoi(parts[2])
	if err != nil || pid <= 1 {
		return "", errors.New("invalid process identity")
	}
	// Revalidate the NUL-separated command line immediately before signaling.
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", err
	}
	if strings.Join(strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), " ") != item.Target {
		return "", errors.New("process identity changed; rescan before stopping it")
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return "", err
	}
	return "killed", nil
}

func (s *SystemService) trashPath() string { return filepath.Join(dataDir, "system-trash.json") }
func (s *SystemService) loadTrash() []TrashEntry {
	data, err := os.ReadFile(s.trashPath())
	if err != nil {
		return []TrashEntry{}
	}
	var out []TrashEntry
	if json.Unmarshal(data, &out) != nil {
		return []TrashEntry{}
	}
	return out
}
func (s *SystemService) writeTrash(items []TrashEntry) error {
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err := AtomicWriteFile(s.trashPath(), data, 0o600, FileOwner{UID: fileUID, GID: fileGID}); err != nil {
		return err
	}
	return nil
}
func (s *SystemService) appendTrash(entry TrashEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeTrash(append([]TrashEntry{entry}, s.loadTrash()...))
}

func (s *SystemService) Delete(ctx context.Context, requested SysItem) error {
	item, err := s.currentItem(ctx, requested)
	if err != nil {
		return err
	}
	switch item.Type {
	case "cron":
		view, _ := logicalToView(item.File)
		data, err := os.ReadFile(view)
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		idx := -1
		original := ""
		for i, line := range lines {
			body := strings.TrimSpace(line)
			if strings.HasPrefix(body, sbOff) {
				body = strings.TrimSpace(strings.TrimPrefix(body, sbOff))
			}
			parsed := parseCronLine(body, item.File, false)
			if parsed != nil && parsed.Schedule == item.Schedule && parsed.Target == item.Target {
				idx = i
				original = line
				break
			}
		}
		if idx < 0 {
			return errors.New("cron entry no longer matches")
		}
		if err := atomicReplaceHostFile(item.File, []byte(strings.Join(append(lines[:idx], lines[idx+1:]...), "\n")), 0o644); err != nil {
			return err
		}
		return s.appendTrash(TrashEntry{TS: time.Now().UTC().Format(time.RFC3339Nano), Type: item.Type, Name: item.Name, File: item.File, Schedule: item.Schedule, Target: item.Target, Line: original, Restorable: true})
	case "systemd-service", "systemd-timer", "systemd-path":
		view, _ := logicalToView(item.File)
		data, err := os.ReadFile(view)
		if err != nil {
			return err
		}
		if s.runner != nil {
			_ = s.runner.Run(ctx, "/usr/bin/env", "systemctl", "disable", "--now", item.Name)
		}
		if err := os.Remove(view); err != nil {
			return err
		}
		if s.runner != nil {
			_ = s.runner.Run(ctx, "/usr/bin/env", "systemctl", "daemon-reload")
		}
		return s.appendTrash(TrashEntry{TS: time.Now().UTC().Format(time.RFC3339Nano), Type: item.Type, Name: item.Name, File: item.File, Schedule: item.Schedule, Target: item.Target, Unit: string(data), Restorable: true})
	case "inotify-proc":
		_, err := killInotify(item)
		if err != nil {
			return err
		}
		return s.appendTrash(TrashEntry{TS: time.Now().UTC().Format(time.RFC3339Nano), Type: item.Type, Name: item.Name, File: item.File, Target: item.Target, Restorable: false})
	}
	return errors.New("unsupported system item")
}

func (s *SystemService) Restore(ctx context.Context, entry TrashEntry) error {
	if !entry.Restorable {
		return errors.New("item is not restorable")
	}
	switch {
	case entry.Type == "cron":
		view, err := logicalToView(entry.File)
		if err != nil {
			return err
		}
		data, _ := os.ReadFile(view)
		content := string(data)
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += entry.Line + "\n"
		if err := atomicReplaceHostFile(entry.File, []byte(content), 0o644); err != nil {
			return err
		}
	case strings.HasPrefix(entry.Type, "systemd-"):
		if err := atomicReplaceHostFile(entry.File, []byte(entry.Unit), 0o644); err != nil {
			return err
		}
		if s.runner != nil {
			res := s.runner.Run(ctx, "/usr/bin/env", "systemctl", "daemon-reload")
			if res.Err != nil {
				return res.Err
			}
		}
	default:
		return errors.New("unsupported trash item")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.loadTrash()
	kept := current[:0]
	for _, item := range current {
		if item.TS == entry.TS && item.Type == entry.Type && item.File == entry.File {
			continue
		}
		kept = append(kept, item)
	}
	return s.writeTrash(kept)
}

func (a *App) handleSystemScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.System == nil {
		http.Error(w, "system service unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, a.System.Scan(r.Context()))
}
func (a *App) handleImportScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.System == nil {
		http.Error(w, "system service unavailable", http.StatusServiceUnavailable)
		return
	}
	found, paths := a.System.ScanImports(r.Context())
	writeJSON(w, map[string]any{"found": found, "paths": paths})
}
func (a *App) handleSystemToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var item SysItem
	if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	state, err := a.System.Toggle(r.Context(), item)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	writeJSON(w, map[string]string{"state": state})
}
func (a *App) handleSystemDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Items   []SysItem `json:"items"`
		Confirm string    `json:"confirm"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || strings.ToLower(strings.TrimSpace(body.Confirm)) != "delete" {
		http.Error(w, "confirmation required", 400)
		return
	}
	type result struct {
		Name  string `json:"name"`
		State string `json:"state"`
		Error string `json:"error,omitempty"`
	}
	out := make([]result, 0, len(body.Items))
	for _, item := range body.Items {
		if err := a.System.Delete(r.Context(), item); err != nil {
			out = append(out, result{Name: item.Name, State: "error", Error: err.Error()})
		} else {
			out = append(out, result{Name: item.Name, State: "deleted"})
		}
	}
	writeJSON(w, out)
}
func (a *App) handleSystemTrash(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, a.System.loadTrash())
}
func (a *App) handleSystemRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Items []TrashEntry `json:"items"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	type result struct {
		Name  string `json:"name"`
		State string `json:"state"`
		Error string `json:"error,omitempty"`
	}
	out := make([]result, 0, len(body.Items))
	for _, item := range body.Items {
		if err := a.System.Restore(r.Context(), item); err != nil {
			out = append(out, result{Name: item.Name, State: "error", Error: err.Error()})
		} else {
			out = append(out, result{Name: item.Name, State: "restored"})
		}
	}
	writeJSON(w, out)
}
