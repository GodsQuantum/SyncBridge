package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Filtre motif + signature de dossier (cœur du watcher NFS-safe).
func TestWatchGlobAndSig(t *testing.T) {
	match := func(globs []string) func(string) bool {
		return func(name string) bool {
			if len(globs) == 0 {
				return true
			}
			for _, g := range globs {
				if ok, _ := filepath.Match(g, filepath.Base(name)); ok {
					return true
				}
			}
			return false
		}
	}
	mp4 := match([]string{"*.mp4"})
	if !mp4("/x/clip.mp4") || mp4("/x/note.md") {
		t.Fatal("filtre *.mp4 incorrect")
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.mp4"), []byte("aa"), 0644)
	s1, err := directorySignature(context.Background(), dir, []string{"*.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "b.md"), []byte("ignored"), 0644)
	if s2, err := directorySignature(context.Background(), dir, []string{"*.mp4"}); err != nil || s2 != s1 {
		t.Fatalf("un .md filtré change la signature: %q %v", s2, err)
	}
	os.WriteFile(filepath.Join(dir, "c.mp4"), []byte("cc"), 0644)
	if s3, err := directorySignature(context.Background(), dir, []string{"*.mp4"}); err != nil || s3 == s1 {
		t.Fatal("un nouveau .mp4 devrait changer la signature")
	}
	if _, err := directorySignature(context.Background(), "/nfs/monte/perdu/zzz", []string{"*.mp4"}); err == nil {
		t.Fatal("dossier absent doit remonter une erreur")
	}
}

// Garde anti-catastrophe rsync : détecter source vide / démontée.
func TestDirIsEmptyOrMissing(t *testing.T) {
	if !dirIsEmptyOrMissing("/n/existe/pas/du/tout") {
		t.Fatal("dossier absent (montage perdu) doit être détecté comme vide/manquant")
	}
	empty := t.TempDir()
	if !dirIsEmptyOrMissing(empty) {
		t.Fatal("dossier vide doit être détecté")
	}
	full := t.TempDir()
	os.WriteFile(filepath.Join(full, "f"), []byte("x"), 0644)
	if dirIsEmptyOrMissing(full) {
		t.Fatal("dossier non vide ne doit PAS être signalé")
	}
}

// Vérifie que le branchement Kind route bien vers le bon moteur.
func TestBuildCmdKind(t *testing.T) {
	// Job commande -> sh -c "<command>"
	c := buildCmd(&Job{Kind: "command", Command: "docker system prune -f"}, false)
	if len(c) != 3 || c[0] != "sh" || c[1] != "-c" || c[2] != "docker system prune -f" {
		t.Fatalf("command: attendu [sh -c ...], obtenu %v", c)
	}

	// Job sync (Kind vide = rétrocompat) -> rsync
	c = buildCmd(&Job{Source: "/a", Dest: "/b", Engine: "rsync", Mode: "mirror"}, false)
	if c[0] != "rsync" {
		t.Fatalf("sync: attendu rsync, obtenu %v", c[0])
	}

	// validateJob : commande vide refusée, cron 5 champs exigé
	if validateJob(&Job{Kind: "command", Command: ""}) == "" {
		t.Fatal("commande vide aurait dû être refusée")
	}
	if validateJob(&Job{Kind: "command", Command: "ls", Trigger: "cron", Cron: "bad"}) == "" {
		t.Fatal("cron invalide aurait dû être refusé")
	}
	if validateJob(&Job{Kind: "command", Command: "ls", Trigger: "manual"}) != "" {
		t.Fatal("job commande manuel valide refusé à tort")
	}
}

func TestHostFilesystemPathUsesPIDOneRoot(t *testing.T) {
	for input, want := range map[string]string{
		"/":           "/proc/1/root",
		"/srv/data":   "/proc/1/root/srv/data",
		"/mnt/media/": "/proc/1/root/mnt/media",
	} {
		got, err := hostFilesystemPath(input)
		if err != nil || got != want {
			t.Fatalf("hostFilesystemPath(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := hostFilesystemPath("relative/path"); err == nil {
		t.Fatal("relative host path accepted")
	}
}
