package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

func checkHealth(ctx context.Context, url string) error {
	if ctx == nil {
		return errors.New("healthcheck context is nil")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	return nil
}

func runHealthcheck(ctx context.Context) error {
	return checkHealth(ctx, "http://127.0.0.1:"+env("SB_PORT", "8787")+"/api/auth/status")
}
