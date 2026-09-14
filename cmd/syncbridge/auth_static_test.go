package main

import (
	"io"
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
