// history.go — historique persisté des runs par job (/config/history.json).
// Aujourd'hui les logs live sont perdus au reboot ; ici on garde une trace légère
// (horodatage, statut, durée, note) des N derniers runs de chaque job.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

const histKeep = 20 // runs conservés par job

type RunRecord struct {
	TS     string `json:"ts"`
	Status string `json:"status"` // ok | error | arrêté
	Dur    int    `json:"dur"`    // secondes
	Note   string `json:"note"`   // 1re ligne d'erreur éventuelle (tronquée)
}

var histMu sync.Mutex

func histPath() string { return dataDir + "/history.json" }

func loadHistory() map[string][]RunRecord {
	m := map[string][]RunRecord{}
	b, err := os.ReadFile(histPath())
	if err != nil {
		return m
	}
	json.Unmarshal(b, &m)
	if m == nil {
		m = map[string][]RunRecord{}
	}
	return m
}

func appendHistory(id int, rec RunRecord) {
	_ = appendHistoryAtomic(id, rec)
}

func appendHistoryAtomic(id int, rec RunRecord) error {
	return appendHistoryAtomicAt(histPath(), FileOwner{UID: fileUID, GID: fileGID}, id, rec)
}

func appendHistoryAtomicAt(path string, owner FileOwner, id int, rec RunRecord) error {
	histMu.Lock()
	defer histMu.Unlock()
	m, err := loadHistoryAt(path)
	if err != nil {
		return err
	}
	key := strconv.Itoa(id)
	list := append([]RunRecord{rec}, m[key]...) // plus récent en tête
	if len(list) > histKeep {
		list = list[:histKeep]
	}
	m[key] = list
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return AtomicWriteFile(path, b, 0o640, owner)
}

func loadHistoryAt(path string) (map[string][]RunRecord, error) {
	m := map[string][]RunRecord{}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string][]RunRecord{}, nil
	}
	return m, nil
}

// removeHistory : purge l'historique d'un job supprimé.
func removeHistory(id int) {
	histMu.Lock()
	defer histMu.Unlock()
	m := loadHistory()
	delete(m, strconv.Itoa(id))
	b, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		b = append(b, '\n')
		_ = AtomicWriteFile(histPath(), b, 0o640, FileOwner{UID: fileUID, GID: fileGID})
	}
}

// apiJobHistory : GET /api/jobs/{id}/history -> derniers runs.
func apiJobHistory(w http.ResponseWriter, id int) {
	m := loadHistory()
	list := m[strconv.Itoa(id)]
	if list == nil {
		list = []RunRecord{}
	}
	writeJSON(w, list)
}

// errNote : 1re ligne parlante d'un run en échec (pour l'historique), tronquée.
func errNote(lines []string) string {
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		low := strings.ToLower(t)
		if strings.Contains(low, "erreur") || strings.Contains(low, "error") ||
			strings.Contains(low, "failed") || strings.Contains(low, "abandon") {
			if len(t) > 160 {
				t = t[:160] + "…"
			}
			return t
		}
	}
	return ""
}
