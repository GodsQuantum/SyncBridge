package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxImportEntries  = 100000
	maxImportFileSize = 512 * 1024
)

// Found describes a simple rsync/rclone command discovered in host text files.
type Found struct {
	Engine  string `json:"engine"`
	Verb    string `json:"verb"`
	Source  string `json:"source"`
	Dest    string `json:"dest"`
	Cron    string `json:"cron"`
	File    string `json:"file"`
	Line    string `json:"line"`
	Local   bool   `json:"local"`
	Warning string `json:"warning"`
}

func importPaths() []string {
	if raw := strings.TrimSpace(os.Getenv("SB_IMPORT_PATHS")); raw != "" {
		var out []string
		for _, item := range strings.Split(raw, ":") {
			item = strings.TrimSpace(item)
			if filepath.IsAbs(item) {
				out = append(out, filepath.Clean(item))
			}
		}
		return out
	}
	// Safe, distro-common sources. Additional script trees are opt-in with SB_IMPORT_PATHS.
	return []string{"/etc/crontab", "/etc/cron.d", "/var/spool/cron/crontabs"}
}

func (s *SystemService) ScanImports(ctx context.Context) ([]Found, []string) {
	paths := importPaths()
	found := []Found{}
	seen := map[string]bool{}
	entries := 0
	for _, logicalRoot := range paths {
		if ctx.Err() != nil || entries >= maxImportEntries {
			break
		}
		root, err := hostFilesystemPath(logicalRoot)
		if err != nil {
			continue
		}
		_ = filepath.WalkDir(root, func(viewPath string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				return nil
			}
			entries++
			if entries > maxImportEntries {
				return fs.SkipAll
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() > maxImportFileSize {
				return nil
			}
			data, err := os.ReadFile(viewPath)
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(root, viewPath)
			if err != nil {
				return nil
			}
			logicalPath := logicalRoot
			if rel != "." {
				logicalPath = filepath.Join(logicalRoot, rel)
			}
			for _, line := range strings.Split(string(data), "\n") {
				item := parseSyncLine(line, logicalPath)
				if item == nil {
					continue
				}
				key := item.Engine + "\x00" + item.Source + "\x00" + item.Dest
				if seen[key] {
					continue
				}
				seen[key] = true
				found = append(found, *item)
			}
			return nil
		})
	}
	return found, paths
}

func parseSyncLine(line, file string) *Found {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, "#") {
		return nil
	}
	cronExpr := ""
	fields := strings.Fields(l)
	if len(fields) >= 6 {
		valid := true
		for i := 0; i < 5; i++ {
			if !isCronField(fields[i]) {
				valid = false
				break
			}
		}
		if valid {
			cronExpr = strings.Join(fields[:5], " ")
		}
	}
	var found *Found
	if idx := strings.Index(l, "rclone "); idx >= 0 {
		found = parseRclone(l[idx:])
	} else if idx := strings.Index(l, "rsync "); idx >= 0 {
		found = parseRsync(l[idx:])
	}
	if found == nil {
		return nil
	}
	found.Cron = cronExpr
	found.File = file
	found.Line = l
	if len(found.Line) > 200 {
		found.Line = found.Line[:200] + "…"
	}
	found.Local = !looksRemote(found.Source) && !looksRemote(found.Dest)
	if !found.Local {
		found.Warning = "remote endpoint detected; review before importing"
	}
	return found
}

func parseRclone(s string) *Found {
	tokens := tokenize(s)
	if len(tokens) < 2 {
		return nil
	}
	verb := tokens[1]
	switch verb {
	case "sync", "copy", "move", "copyto", "moveto":
	default:
		return nil
	}
	args := nonOptionArgs(tokens[2:])
	if len(args) < 2 {
		return nil
	}
	return &Found{Engine: "rclone", Verb: verb, Source: args[0], Dest: args[1]}
}

func parseRsync(s string) *Found {
	tokens := tokenize(s)
	if len(tokens) < 2 {
		return nil
	}
	args := nonOptionArgs(tokens[1:])
	if len(args) < 2 {
		return nil
	}
	return &Found{Engine: "rsync", Source: args[0], Dest: args[len(args)-1]}
}

func tokenize(s string) []string {
	var out []string
	var current strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

func nonOptionArgs(tokens []string) []string {
	var out []string
	for _, token := range tokens {
		if strings.HasPrefix(token, "-") {
			continue
		}
		switch token {
		case "|", ">", ">>", "&&", ";":
			return out
		}
		out = append(out, token)
	}
	return out
}

func looksRemote(path string) bool {
	if strings.Contains(path, "@") && strings.Contains(path, ":") {
		return true
	}
	return strings.Index(path, ":") > 0 && !strings.HasPrefix(path, "/")
}

func isCronField(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r == '*' || r == '/' || r == ',' || r == '-' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}
