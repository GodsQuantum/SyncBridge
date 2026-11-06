package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateProgressReadOnlyAndAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	app := &App{UpdateProgressPath: path}
	protected := requireAuth(app.Handler())
	bad := httptest.NewRecorder()
	protected.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/update-progress", nil))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET status=%d", bad.Code)
	}
	absent := httptest.NewRecorder()
	app.Handler().ServeHTTP(absent, httptest.NewRequest(http.MethodGet, "/api/v1/update-progress", nil))
	if absent.Code != 200 || !strings.Contains(absent.Body.String(), `"rows":[]`) {
		t.Fatalf("absent=%d %s", absent.Code, absent.Body.String())
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"updatedAt":"2026-11-06T14:00:00Z","backupLocalFrozen":true,"counts":{"available":1},"rows":[{"id":"ct1:test","target":"CT1","kind":"Docker","component":"test","image":"demo","status":"available","note":"safe","checkedAt":null}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	ok := httptest.NewRecorder()
	app.Handler().ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/api/v1/update-progress", nil))
	if ok.Code != 200 || !strings.Contains(ok.Body.String(), `"backupLocalFrozen":true`) || ok.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected GET %d %s", ok.Code, ok.Body.String())
	}
	method := httptest.NewRecorder()
	app.Handler().ServeHTTP(method, httptest.NewRequest(http.MethodPost, "/api/v1/update-progress", nil))
	if method.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", method.Code)
	}
	if err := os.WriteFile(path, []byte("bad json"), 0644); err != nil {
		t.Fatal(err)
	}
	broken := httptest.NewRecorder()
	app.Handler().ServeHTTP(broken, httptest.NewRequest(http.MethodGet, "/api/v1/update-progress", nil))
	if broken.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken=%d", broken.Code)
	}
}
