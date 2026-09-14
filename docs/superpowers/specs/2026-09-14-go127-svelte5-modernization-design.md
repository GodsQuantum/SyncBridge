# SyncBridge Go 1.27 + Svelte 5 modernization design

## Goal
Modernize SyncBridge without changing its core deployment philosophy: one hardened Go controller binary, no Node runtime in production, host-native execution, and a compact responsive UI.

## Baseline
- Canonical repository starts at `836c329`.
- Existing backend is Go 1.26.7 and already has schema-v2 jobs, API v1, resumable SSE run events, legacy compatibility handlers, and extensive Go tests.
- Existing UI is a monolithic vanilla JS/HTML frontend using compatibility endpoints and a 2-second `/api/status` poll.
- CI runs on Ubuntu 24.04. Local tests must use a GNU userland; Alpine/BusyBox is not a valid test environment for wrapper tests using `readlink -m`.

## Target versions (2026-09-14)
- Go 1.27.1.
- Svelte 5.57.0.
- SvelteKit 2.70.3.
- adapter-static 3.0.10.
- Vite 8.3.0.
- TypeScript 7.0.2.
- Node 26.8.2 for build tooling only.

## Architecture
`webui/` contains the SvelteKit source. It is prerendered with `adapter-static` into `cmd/syncbridge/web/`, which remains embedded into the Go binary with `go:embed`. The final runtime image contains no Node executable or `node_modules`.

The frontend uses a typed API client. Core job CRUD/run state uses `/api/v1/*`; existing system-import/remotes/settings/auth compatibility endpoints remain temporarily because they do not yet have v1 equivalents. The UI must never monkey-patch global `fetch` or `EventSource`; remote-instance routing belongs in the API client.

Run state is event-driven. `/api/v1/events` is the primary live channel. An initial snapshot uses `/api/v1/jobs` plus recent runs. Polling is retained only as a low-frequency recovery/remote-health mechanism if needed, never as the primary 2-second live loop.

## UI structure
- `Dashboard`: search, filters, instance selector, job cards.
- `JobCard`: status, action summary, trigger, run controls, compact live output/history.
- `JobEditor`: job type/action, scheduler ownership, identity, trigger, sync options, folder browser, advanced safety controls.
- `SystemImportModal`: system inventory/import/trash flow using existing endpoints.
- `InstancesModal`: local-only remote management.
- `SettingsModal`: notification settings.
- `Auth`: login/register page generated as `auth.html` to preserve the current Go auth middleware contract.
- `Toast` and reusable `Modal` primitives.

The first migration preserves feature parity and the current visual identity, then improves hierarchy, responsive behavior, focus states, empty/loading/error states, destructive-action affordances, and live-run visibility.

## API behavior
The frontend reads native schema-v2 `Job` objects. Update and delete operations send `If-Match` with the current revision. Toggle and clone are implemented client-side from v1 primitives. Run start uses `POST /api/v1/jobs/{id}/runs`; stop uses `POST /api/v1/runs/{runId}/stop`.

Persisted legacy history is exposed through a small new v1 endpoint under `/api/v1/jobs/{id}/history` so the new UI does not need the legacy job history route. System/remotes/settings/auth stay compatibility endpoints until a later versioned-API project.

## Build and release
Docker becomes a multi-stage Node -> Go -> Alpine build. `npm ci && npm run build` produces the embedded static assets before Go compilation. CI builds/tests the frontend, runs Go format/tidy/vet/test/race on Go 1.27.1, validates Compose, then performs the existing image smoke test.

Generated frontend assets stay committed so a source checkout can still compile Go without requiring Node first; CI verifies they are reproducible from `webui/`.

## Testing
- Pure TypeScript tests for formatting, job mapping, API prefixing and event reduction.
- Svelte component tests for primary interactions where practical.
- Existing Go suite remains mandatory.
- New Go tests cover the v1 persisted-history route and static asset/auth contract.
- Production verification includes Svelte check/build, Go vet/test/race, container image build, and HTTP smoke tests.

## Workspace policy
- `/home/arezki/Documents/Projets/HomeLab/SyncBridge/sync` is the canonical clean `main` checkout synced to GitHub.
- Development occurs in `/home/arezki/Documents/Projets/HomeLab/SyncBridge/.worktrees/modernize` on `feat/go127-svelte5`.
- Temporary investigation artifacts stay under the project workspace and are deleted when no longer useful.
- Historical duplicate trees/archives are removed only after confirming they contain no unique useful state.
