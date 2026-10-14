# Rust uutils toolchain support — Implementation Plan

> **For agentic workers:** Use superpowers:test-driven-development and superpowers:verification-before-completion. Work in the isolated LXC 700 Git worktree.

**Goal:** Make SyncBridge fully usable on Ubuntu 26.04 Rust uutils hosts without sacrificing its host command validation.

**Architecture:** Add a guarded read-only `stat --version` fallback to the existing toolchain probe, and add one supported profile constant with explicit semantic checks. Keep all existing security constraints and jobs unchanged.

**Tech Stack:** Go 1.27.2, existing test runner and GitHub Actions, Docker/Svelte 5 baseline.

**Spec:** `docs/superpowers/specs/2026-10-14-uutils-toolchain-compatibility-design.md`

## Global constraints
- Source: `/home/arezki/Projets/HomeLab/SyncBridge/sync` inside LXC 700.
- No modifications to Proxmox or NFS mounts; preserve original files and /config.
- Do not weaken `host_toolchain` syntax probing or auth controls.
- CI build/release must be green prior to LXC 200 deployment.
- Existing cron job 4 and its data must survive unchanged.

## Review focus
- Spoofed `stat --version` never executes arbitrary text.
- Missing `sync -f` / `mv -T` remains rejected.
- GNU and BusyBox remain accepted.
- Unknown toolchains remain rejected.
- Real uutils `0.10.0` passes when options are actually supported.

### Task 1 — Baseline and TDD
- [ ] Verify worktree checkout and green baseline.
- [ ] Extend `cmd/syncbridge/capabilities_test.go` fixture/profile matrix for uutils; observe test failure.
- [ ] Make minimum code change in `cmd/syncbridge/capabilities.go`; run RED/GREEN and negative tests.
- [ ] Validate `gofmt`, `go test ./...`, `go vet ./...`, Go build.

### Task 2 — Real uutils compatibility and release
- [ ] Test capability probing inside the LXC 700 actual uutils 0.10.0 environment; preserve no-mutation constraints.
- [ ] Document verified scope and tradeoffs.
- [ ] Review git diff, commit, push main, inspect CI and published multiarch image.

### Task 3 — Safe Cloud9 roll-out
- [ ] Capture current LXC 200 compose/config/image details and test deployment fallback.
- [ ] Pull verified upstream latest, restart only SyncBridge, confirm UI/session config and job 4 present.
- [ ] Check capability report & run job 4, verify HDD mirroring and Syncthing rescan succeed.
- [ ] Clean temporary test worktrees/builds, verify owner's UID/GID 1000 and record handoff.
