package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestComposeHostExecutorInvariants(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker unavailable: semantic Compose render is mandatory in CI")
	}
	root := repositoryRoot(t)
	compose := filepath.Join(root, "deploy", "compose.yaml")
	envFile := filepath.Join(root, "deploy", "syncbridge.env.example")
	cmd := exec.Command(docker, "compose", "--env-file", envFile, "-f", compose, "config", "--format", "json")
	cmd.Env = append(os.Environ(), "COMPOSE_ANSI=never")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose render failed: %v\n%s", err, out)
	}
	var cfg map[string]any
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("compose returned invalid JSON: %v\n%s", err, out)
	}
	if got := fmt.Sprint(cfg["name"]); got != "syncbridge" {
		t.Errorf("project = %q, want syncbridge", got)
	}
	services, ok := cfg["services"].(map[string]any)
	if !ok {
		t.Fatal("compose has no services object")
	}
	svc, ok := services["syncbridge"].(map[string]any)
	if !ok {
		t.Fatal("compose has no syncbridge service")
	}
	if fmt.Sprint(svc["user"]) != "0:0" || fmt.Sprint(svc["pid"]) != "host" || svc["read_only"] != true {
		t.Errorf("invalid root/pid/read-only contract: user=%v pid=%v read_only=%v", svc["user"], svc["pid"], svc["read_only"])
	}
	if privileged, _ := svc["privileged"].(bool); privileged {
		t.Error("compose must never be privileged")
	}
	if !containsRenderedString(svc["cap_add"], "SYS_ADMIN") {
		t.Error("compose missing SYS_ADMIN")
	}
	if !containsAnyRenderedString(svc["security_opt"], "no-new-privileges:true", "no-new-privileges=true", "no-new-privileges") {
		t.Error("compose missing no-new-privileges")
	}
	envMap, _ := svc["environment"].(map[string]any)
	if fmt.Sprint(envMap["SB_DATA"]) != "/config" || fmt.Sprint(envMap["SB_PORT"]) != "8787" {
		t.Errorf("environment mismatch: %v", envMap)
	}
	for key := range envMap {
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") {
			t.Errorf("compose embeds secret-like environment key %q", key)
		}
	}
	volumes, _ := svc["volumes"].([]any)
	if len(volumes) != 1 {
		t.Fatalf("volumes = %d, want only /config", len(volumes))
	}
	volume, _ := volumes[0].(map[string]any)
	if fmt.Sprint(volume["source"]) != "/opt/syncbridge" || fmt.Sprint(volume["target"]) != "/config" {
		t.Errorf("volume = %v, want /opt/syncbridge:/config", volume)
	}
	ports, _ := svc["ports"].([]any)
	published := ""
	for _, raw := range ports {
		if port, ok := raw.(map[string]any); ok && fmt.Sprint(port["target"]) == "8787" {
			published = fmt.Sprint(port["published"])
		}
	}
	if published != "8787" {
		t.Errorf("published port = %q, want 8787", published)
	}
}

func containsRenderedString(value any, want string) bool {
	return containsAnyRenderedString(value, want)
}

func containsAnyRenderedString(value any, wants ...string) bool {
	items, _ := value.([]any)
	for _, item := range items {
		got := fmt.Sprint(item)
		for _, want := range wants {
			if got == want {
				return true
			}
		}
	}
	return false
}

func TestDockerfileHostExecutorRuntime(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repositoryRoot(t), "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"FROM golang:1.26.7-alpine3.24@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build",
		"FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b",
		"util-linux-misc",
		"ca-certificates",
		"tzdata",
		"HEALTHCHECK",
		"syncbridge\", \"healthcheck",
		"go build -trimpath -ldflags=\"-s -w -buildid=\" -o /out/syncbridge ./cmd/syncbridge",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
	for _, forbidden := range []string{"\n    rsync", "\n    rclone", "\n    bash", "\n    curl", "\n    jq", "\n    findutils", "docker-cli", "openssh-client"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("Dockerfile still contains host tool %q", forbidden)
		}
	}
}

func TestDeploymentExampleIsGeneric(t *testing.T) {
	root := repositoryRoot(t)
	envPath := filepath.Join(root, "deploy", "syncbridge.env.example")
	b, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"COMPOSE_PROJECT_NAME=syncbridge",
		"SYNCBRIDGE_HOST_PORT=8787",
		"SYNCBRIDGE_CONFIG_DIR=/opt/syncbridge",
		"TZ=UTC",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("env example missing %q", want)
		}
	}
	for _, forbidden := range []string{"192.168.", "/home/", "container_name:", "networks:"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("env example contains private/machine-specific marker %q", forbidden)
		}
	}
}

func TestPublishWorkflowPinsActionsAndGatesImage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github", "workflows", "publish.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
		"actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
		"docker/setup-qemu-action@96fe6ef7f33517b61c61be40b68a1882f3264fb8",
		"docker/setup-buildx-action@bb05f3f5519dd87d3ba754cc423b652a5edd6d2c",
		"docker/login-action@dbcb813823bdd20940b903addbd779551569679f",
		"docker/metadata-action@dc802804100637a589fabce1cb79ff13a1411302",
		"docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a",
		"docker compose --env-file deploy/syncbridge.env.example -f deploy/compose.yaml config",
		"go test -race",
		"node --check cmd/syncbridge/web/app.js",
		"test -r /proc/1/root/etc/os-release",
		"/proc/1/root/tmp/.syncbridge-smoke-",
		"needs: verify",
		"platforms: linux/amd64,linux/arm64",
		"provenance: mode=max",
		"sbom: true",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("publish workflow missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"actions/checkout@v",
		"actions/setup-go@v",
		"docker/setup-qemu-action@v",
		"docker/setup-buildx-action@v",
		"docker/login-action@v",
		"docker/metadata-action@v",
		"docker/build-push-action@v",
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("publish workflow uses mutable action reference %q", forbidden)
		}
	}
}
