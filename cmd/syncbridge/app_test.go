package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseFileOwnerRejectsInvalidIDs(t *testing.T) {
	for _, tc := range []struct{ uid, gid string }{{"-1", "1000"}, {"1000", "-1"}, {"x", "1000"}, {"1000", "x"}} {
		if _, err := parseFileOwner(tc.uid, tc.gid); err == nil {
			t.Fatalf("accepted uid=%q gid=%q", tc.uid, tc.gid)
		}
	}
	owner, err := parseFileOwner("0", "42")
	if err != nil || owner.UID != 0 || owner.GID != 42 {
		t.Fatalf("owner=%#v err=%v", owner, err)
	}
}

func TestInstanceIDPersistsAndIsValid(t *testing.T) {
	dir := t.TempDir()
	owner := FileOwner{UID: os.Getuid(), GID: os.Getgid()}
	one, err := loadOrCreateInstanceID(dir, owner)
	if err != nil {
		t.Fatal(err)
	}
	two, err := loadOrCreateInstanceID(dir, owner)
	if err != nil {
		t.Fatal(err)
	}
	if one != two || !instanceIDPattern.MatchString(one) {
		t.Fatalf("ids=%q %q", one, two)
	}
	data, err := os.ReadFile(filepath.Join(dir, "instance-id"))
	if err != nil || string(data) != one+"\n" {
		t.Fatalf("file=%q err=%v", data, err)
	}
}

func TestHandlerExposesHostSystemRoutesWhenConfigured(t *testing.T) {
	app := &App{System: NewSystemService(nil)}
	handler := app.Handler()
	for _, path := range []string{
		"/api/import/scan",
		"/api/system/scan",
		"/api/system/trash",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s unexpectedly 404", path)
		}
	}
}
