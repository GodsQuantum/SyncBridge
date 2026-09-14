# SyncBridge Go 1.27 + Svelte 5 Modernization Implementation Plan

> **For agentic workers:** execute task-by-task with tests before behavior changes.

**Goal:** Upgrade the backend toolchain to Go 1.27.1 and replace the monolithic frontend with a typed Svelte 5/SvelteKit static UI while preserving runtime simplicity and feature parity.

**Architecture:** SvelteKit prerenders into the Go embedded web directory. Core job/run traffic uses API v1 and global SSE; legacy endpoints remain only for subsystems without v1 equivalents.

**Tech Stack:** Go 1.27.1, Svelte 5.57.0, SvelteKit 2.70.3, adapter-static 3.0.10, Vite 8.3.0, TypeScript 7.0.2, Vitest 5.0.0.

**Spec:** `docs/superpowers/specs/2026-09-14-go127-svelte5-modernization-design.md`

## Global Constraints
- No Node runtime in the final image.
- Preserve current host-executor/security model.
- Keep `sync/main` clean until the feature branch is verified.
- API v1 mutation revision semantics are mandatory.
- Preserve auth/register, remotes, system import/trash, notifications, folder browsing, job editing, dry-run, live logs and stop controls.

---

### Task 1: Toolchain and build baseline
- Update Go directive/toolchain, direct Go dependencies, Docker builder and CI matrix to Go 1.27.1.
- Add Node/Svelte build stage without changing final runtime contents.
- Verify Go suite on GNU userland and module integrity.

### Task 2: Frontend project and test harness
- Add `webui/` SvelteKit static project with exact dependency pins.
- Configure static generation into `cmd/syncbridge/web` and preserve `auth.html`.
- Add Vitest/jsdom and reusable frontend test helpers.

### Task 3: Typed API client and state reducers
- Test then implement remote-aware API routing without global monkey patches.
- Test then implement v2 job normalization/edit payload helpers.
- Test then implement run-event state/log reduction and formatting helpers.

### Task 4: API v1 gaps
- Add failing Go tests then expose persisted job history under v1.
- Keep compatibility routes functional.

### Task 5: Dashboard and live jobs
- Build dashboard shell, filters/search, instance selector and job cards.
- Use API v1 jobs/runs and SSE live state/logs.
- Implement run, dry-run, stop, enable/disable, clone, delete and history with revision safety.

### Task 6: Job editor and folder browser
- Build typed editor for sync/command jobs, triggers, scheduler owner, identity and advanced safety options.
- Preserve import-prefill behavior and folder browsing.

### Task 7: System import, remotes, settings and auth
- Port system inventory/import/trash flows.
- Port remote-instance management and health.
- Port notification settings and login/register.

### Task 8: UI/debug pass
- Audit responsive layout, focus/keyboard flow, modal behavior, loading/error/empty states and destructive actions.
- Remove dead legacy frontend code and 2-second status polling.
- Address reproducible defects found during the pass.

### Task 9: Verification, cleanup and handoff
- Run frontend check/tests/build.
- Run Go fmt/tidy/vet/test/race on Go 1.27.1.
- Build and smoke-test the container.
- Review git diff for secrets/personal data/generated junk.
- Clean obsolete workspace artifacts after uniqueness checks.
- Update workspace handoff and synchronize feature/main according to verified branch state.
