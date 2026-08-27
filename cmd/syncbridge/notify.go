// notify.go — réglages persistés (/config/settings.json) + notifications d'échec.
// Contrat volontairement simple & universel : POST du message en corps texte avec
// un header "Title" -> compatible ntfy nativement, et accepté par la plupart des
// webhooks. Rien à monter en plus, tout vit dans /config.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Settings struct {
	NotifyURL string `json:"notifyUrl"` // ex https://ntfy.sh/mon-topic (vide = désactivé)
	NotifyAll bool   `json:"notifyAll"` // true = notifie aussi les succès, sinon échecs seuls
}

var (
	settingsMu sync.Mutex
	settings   = Settings{}
)

func settingsPath() string { return dataDir + "/settings.json" }

func loadSettings() {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	b, err := os.ReadFile(settingsPath())
	if err != nil {
		return
	}
	json.Unmarshal(b, &settings)
}

func saveSettings() {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	b, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath(), b, 0600)
	own(settingsPath())
}

func getSettings() Settings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	return settings
}

// apiSettings : GET lit, POST met à jour (avec test optionnel).
func apiSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, getSettings())
	case "POST":
		var in struct {
			NotifyURL string `json:"notifyUrl"`
			NotifyAll bool   `json:"notifyAll"`
			Test      bool   `json:"test"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "requête invalide", http.StatusBadRequest)
			return
		}
		settingsMu.Lock()
		settings.NotifyURL = strings.TrimSpace(in.NotifyURL)
		settings.NotifyAll = in.NotifyAll
		settingsMu.Unlock()
		saveSettings()
		testErr := ""
		if in.Test {
			if err := postNotify(settings.NotifyURL, "SyncBridge", "Notification de test ✅"); err != nil {
				testErr = err.Error()
			}
		}
		writeJSON(w, map[string]any{"ok": true, "testError": testErr})
	default:
		http.Error(w, "méthode", http.StatusMethodNotAllowed)
	}
}

// notifyRun : appelé après chaque run (non dry). Best-effort, non bloquant.
func notifyRun(name, status string) {
	s := getSettings()
	if s.NotifyURL == "" {
		return
	}
	if status != "error" && !s.NotifyAll {
		return
	}
	icon := "✅"
	if status == "error" {
		icon = "❌"
	}
	go postNotify(s.NotifyURL, "SyncBridge : "+name, fmt.Sprintf("%s job « %s » : %s", icon, name, status))
}

func postNotify(url, title, body string) error {
	if url == "" {
		return fmt.Errorf("aucune URL de notification configurée")
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)   // ntfy : titre
	req.Header.Set("X-Title", title) // alias
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	cl := &http.Client{Timeout: 8 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("réponse %d du service de notification", resp.StatusCode)
	}
	return nil
}
