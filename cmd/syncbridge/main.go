// SyncBridge — sync local-to-local. Moteur rsync/rclone. Binaire unique Go.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var webFS embed.FS

var dataDir = env("SB_DATA", "/config")

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// UID/GID des fichiers créés par l'app (défaut 1000:1000).
// Même si le conteneur tourne en root, ses fichiers de config restent en 1000.
var (
	fileUID = 1000
	fileGID = 1000
)

func atoiDef(s string, d int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return d
}

// own force le propriétaire d'un fichier/dossier à SB_UID:SB_GID (best-effort).
func own(path string) { _ = os.Chown(path, fileUID, fileGID) }

// ---------- construction commande moteur ----------
func buildCmd(j *Job, dry bool) []string {
	if j.Kind == "command" {
		// Job commande : exécute une commande/script shell arbitraire (scripts CRON,
		// docker prune, export Paperless, utilitaires...). Le dry-run n'a pas de sens
		// pour une commande libre : on le laisse à l'UI de ne pas le proposer.
		return []string{"sh", "-c", j.Command}
	}
	src := strings.TrimRight(j.Source, "/") + "/"
	dst := strings.TrimRight(j.Dest, "/") + "/"

	if j.Engine == "rclone" {
		verb := "copy"
		if j.Mode == "mirror" {
			verb = "sync"
		} else if j.Mode == "move" {
			verb = "move"
		}
		c := []string{"rclone", verb, src, dst, "--progress", "--stats=1s", "--stats-one-line"}
		if j.Compare == "checksum" {
			c = append(c, "--checksum")
		}
		if j.Bwlimit != "" {
			c = append(c, "--bwlimit", strings.TrimSuffix(j.Bwlimit, "B"))
		}
		if j.Backup {
			c = append(c, "--backup-dir", filepath.Join(dst, ".sb-backup", time.Now().Format("20060102-150405")))
			c = append(c, "--exclude", ".sb-backup/**")
		}
		if j.MaxDel > 0 {
			c = append(c, "--max-delete", strconv.Itoa(j.MaxDel))
		}
		if j.SkipNew {
			c = append(c, "--update")
		}
		for _, e := range splitCSV(j.Exclude) {
			c = append(c, "--exclude", e)
		}
		if dry {
			c = append(c, "--dry-run")
		}
		return c
	}

	// rsync
	c := []string{"rsync", "-a", "--info=progress2,stats2", "--human-readable"}
	if j.SysBackup {
		// Backup système fidèle : préserve propriétaires/perms/ACL/xattr/hardlinks.
		// --fake-super : stocke la vraie identité dans les xattr (le fichier reste
		// physiquement 1000 sur le NFS squashé, mais restaurable à l'identique).
		c = append(c, "-HAX", "--numeric-ids", "--fake-super")
	} else {
		// Comportement normal : tout appartient à 1000:1000 partout (même disque local),
		// pas de préservation des propriétaires root.
		c = append(c, "--chown=1000:1000")
	}
	if j.Mode == "mirror" {
		c = append(c, "--delete")
	}
	if j.Mode == "move" {
		c = append(c, "--remove-source-files")
	}
	if j.Compare == "checksum" {
		c = append(c, "--checksum")
	}
	if j.Bwlimit != "" {
		c = append(c, "--bwlimit", strings.TrimSuffix(j.Bwlimit, "B"))
	}
	if j.Backup {
		c = append(c, "--backup", "--backup-dir",
			filepath.Join(dst, ".sb-backup", time.Now().Format("20060102-150405")))
		c = append(c, "--exclude", ".sb-backup") // ne pas syncer/supprimer la corbeille elle-même
	}
	if j.MaxDel > 0 {
		c = append(c, "--max-delete", strconv.Itoa(j.MaxDel))
	}
	if j.SkipNew {
		c = append(c, "--update")
	}
	for _, e := range splitCSV(j.Exclude) {
		c = append(c, "--exclude", e)
	}
	if dry {
		c = append(c, "--dry-run", "--itemize-changes")
	}
	c = append(c, src, dst)
	return c
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// humanSummary : explique en français ce que le job va faire, avant la commande.
func humanSummary(j *Job, dry bool) string {
	if j.Kind == "command" {
		return "• Commande : " + j.Command + "\n"
	}
	var b strings.Builder
	modeTxt := map[string]string{
		"add":    "AJOUTE les nouveaux fichiers et met à jour les modifiés (aucune suppression dans la destination)",
		"mirror": "REND la destination identique à la source (les fichiers absents de la source seront SUPPRIMÉS dans la destination)",
		"move":   "COPIE vers la destination puis VIDE la source",
	}[j.Mode]
	b.WriteString("• Action : " + modeTxt + "\n")
	b.WriteString("• De  : " + j.Source + "\n")
	b.WriteString("• Vers: " + j.Dest + "\n")
	if j.SysBackup {
		b.WriteString("• BACKUP SYSTÈME FIDÈLE : propriétaires/permissions/ACL/hardlinks préservés via xattr (restaurable à l'identique)\n")
	}
	if j.Compare == "checksum" {
		b.WriteString("• Comparaison par checksum (lecture complète du contenu)\n")
	}
	if j.Backup {
		keep := "toutes les versions gardées"
		if j.BackupKeep > 0 {
			keep = fmt.Sprintf("%d dernières versions gardées", j.BackupKeep)
		}
		b.WriteString("• Corbeille de sécurité active (" + keep + ")\n")
	}
	if j.MaxDel > 0 {
		b.WriteString(fmt.Sprintf("• Garde-fou : abandon si plus de %d suppressions\n", j.MaxDel))
	}
	if dry {
		b.WriteString("• SIMULATION : rien n'est modifié. La liste ci-dessous montre ce qui CHANGERAIT.\n")
		b.WriteString("  (colonnes rsync : > = à envoyer, c = créé, d = dossier, * = supprimé)\n")
	}
	return b.String()
}

// rotateBackups : ne garde que les N dossiers .sb-backup les plus récents.
func rotateBackups(dst string, keep int) {
	if keep <= 0 {
		return
	}
	base := filepath.Join(dst, ".sb-backup")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) <= keep {
		return
	}
	sort.Strings(dirs) // noms horodatés YYYYMMDD-HHMMSS -> tri chrono
	for _, old := range dirs[:len(dirs)-keep] {
		os.RemoveAll(filepath.Join(base, old))
		fmt.Printf("[backup] purge ancienne version: %s\n", old)
	}
}

// dirIsEmptyOrMissing : true si le dossier est absent, illisible (montage perdu),
// ou ne contient aucune entrée. Sert de garde anti-catastrophe avant un --delete.
func dirIsEmptyOrMissing(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return true // absent ou inaccessible
	}
	defer f.Close()
	names, err := f.Readdirnames(1) // lit au plus 1 entrée : suffit pour "vide ?"
	if err != nil {
		return true
	}
	return len(names) == 0
}

// ---------- HTTP ----------
func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := runHealthcheck(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "SyncBridge healthcheck failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	owner, err := parseFileOwner(env("SB_UID", "1000"), env("SB_GID", "1000"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "SyncBridge configuration error: %v\n", err)
		os.Exit(2)
	}
	fileUID, fileGID = owner.UID, owner.GID
	dataDir = env("SB_DATA", "/config")
	loadSecret()
	loadSettings()
	go pruneSessions()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := NewProductionApp(ctx, dataDir, owner)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SyncBridge startup error: %v\n", err)
		os.Exit(1)
	}

	addr := ":" + env("SB_PORT", "8787")
	server := &http.Server{
		Addr: addr, Handler: requireAuth(app.Handler()),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	fmt.Printf("SyncBridge démarré | addr %s | data %s | %d job(s)\n", addr, dataDir, len(app.Jobs.List()))

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
		}
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	if err := app.Close(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func apiEngines(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{
		"rsync":  hasBin("rsync"),
		"rclone": hasBin("rclone"),
	})
}

func hasBin(n string) bool { _, err := exec.LookPath(n); return err == nil }

// validateJob : refuse les configurations dangereuses ou incohérentes.
// Renvoie "" si OK, sinon un message d'erreur.
func validateJob(j *Job) string {
	// Backend System : n'a de sens que pour un déclencheur autonome (cron/watch).
	// Le manuel reste forcément côté SyncBridge (c'est SyncBridge qui "lance").
	if j.Backend == "system" && j.Trigger != "cron" && j.Trigger != "watch" {
		return "backend System : choisis un déclencheur cron ou surveillance (le manuel reste côté SyncBridge)"
	}
	if j.Kind == "command" {
		if env("SB_LOCK_COMMANDS", "") == "1" {
			return "création de jobs commande désactivée sur cette instance (SB_LOCK_COMMANDS=1)"
		}
		if strings.TrimSpace(j.Command) == "" {
			return "job commande : la commande est vide"
		}
		if j.Trigger == "cron" && len(strings.Fields(j.Cron)) != 5 {
			return "expression cron invalide : 5 champs attendus (min heure jour mois jour-semaine)"
		}
		return ""
	}
	src := strings.TrimRight(j.Source, "/")
	dst := strings.TrimRight(j.Dest, "/")
	if src == dst && src != "" {
		return "source et destination identiques : refusé (risque de perte de données)"
	}
	// destination imbriquée dans la source en mode miroir = --delete catastrophique
	if j.Mode == "mirror" && src != "" && strings.HasPrefix(dst+"/", src+"/") {
		return "en miroir, la destination ne peut pas être à l'intérieur de la source"
	}
	if j.Trigger == "cron" && len(strings.Fields(j.Cron)) != 5 {
		return "expression cron invalide : 5 champs attendus (min heure jour mois jour-semaine)"
	}
	return ""
}

func defaults(j *Job) {
	if j.Kind == "" {
		j.Kind = "sync"
	}
	if j.Backend == "" {
		j.Backend = "syncbridge"
	}
	if j.Engine == "" {
		j.Engine = "rsync"
	}
	if j.Mode == "" {
		j.Mode = "add"
	}
	if j.Trigger == "" {
		j.Trigger = "manual"
	}
	if j.Compare == "" {
		j.Compare = "time"
	}
}

func apiBrowse(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/mnt"
	}
	viewPath, err := hostFilesystemPath(path)
	if err != nil {
		http.Error(w, "dossier invalide", 400)
		return
	}
	fi, err := os.Stat(viewPath)
	if err != nil || !fi.IsDir() {
		http.Error(w, "dossier invalide", 400)
		return
	}
	entries, err := os.ReadDir(viewPath)
	if err != nil {
		http.Error(w, "accès refusé", 403)
		return
	}
	dirs := []string{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	parent := ""
	if path != "/" {
		parent = filepath.Dir(path)
	}
	writeJSON(w, map[string]any{"path": path, "parent": parent, "dirs": dirs})
}
