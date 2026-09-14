package main

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireAuthAllowsSvelteAssetsButProtectsApp(t *testing.T) {
	handler := requireAuth((&App{}).Handler())

	assetReq := httptest.NewRequest(http.MethodGet, "/_app/version.json", nil)
	assetRec := httptest.NewRecorder()
	handler.ServeHTTP(assetRec, assetReq)
	if assetRec.Code != http.StatusOK {
		t.Fatalf("asset status=%d body=%q", assetRec.Code, assetRec.Body.String())
	}
	if !strings.Contains(assetRec.Body.String(), "version") {
		t.Fatalf("asset body=%q", assetRec.Body.String())
	}

	pageReq := httptest.NewRequest(http.MethodGet, "/", nil)
	pageRec := httptest.NewRecorder()
	handler.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK || !strings.Contains(pageRec.Body.String(), "SyncBridge") {
		t.Fatalf("page status=%d body=%q", pageRec.Code, pageRec.Body.String())
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	apiRec := httptest.NewRecorder()
	handler.ServeHTTP(apiRec, apiReq)
	body, _ := io.ReadAll(apiRec.Result().Body)
	if apiRec.Code != http.StatusUnauthorized {
		t.Fatalf("api status=%d body=%q", apiRec.Code, body)
	}
}

func TestStaticCachePolicy(t *testing.T) {
	matches, err := fs.Glob(webFS, "web/_app/immutable/entry/start.*.js")
	if err != nil || len(matches) != 1 {
		t.Fatalf("immutable asset matches=%v err=%v", matches, err)
	}
	handler := requireAuth((&App{}).Handler())

	assetPath := strings.TrimPrefix(matches[0], "web")
	assetRec := httptest.NewRecorder()
	handler.ServeHTTP(assetRec, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if got := assetRec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("immutable cache-control=%q", got)
	}

	pageRec := httptest.NewRecorder()
	handler.ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := pageRec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("page cache-control=%q", got)
	}

	versionRec := httptest.NewRecorder()
	handler.ServeHTTP(versionRec, httptest.NewRequest(http.MethodGet, "/_app/version.json", nil))
	if got := versionRec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("version cache-control=%q", got)
	}
}
