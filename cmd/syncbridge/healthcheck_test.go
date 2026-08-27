package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckHealthRequiresSuccessfulHTTPResponse(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer good.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := checkHealth(ctx, good.URL); err != nil {
		t.Fatalf("healthy endpoint rejected: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) }))
	defer bad.Close()
	if err := checkHealth(ctx, bad.URL); err == nil {
		t.Fatal("unhealthy endpoint accepted")
	}
}
