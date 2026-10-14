# SyncBridge — Rust uutils Coreutils Compatibility (2026-10-14)

## Intent
Allow the **existing** SyncBridge host executor to run its validated rsync/rclone/script jobs when the underlying Linux host uses Ubuntu 26.04 `coreutils-from-uutils`, without requiring an OS-wide switch to GNU coreutils. Preserve the current hardened execution model, existing jobs, and public API. No speculative subsystems.

## Verified defect
Cloud9 LXC 200 originally had uutils `stat (uutils coreutils) 0.10.0`. The host toolchain probe accepts only `GNU coreutils` or `BusyBox` in `stat --help`, so all SyncBridge jobs failed with `host_toolchain` even though all required binaries and options existed. Temporary operational workaround switched LXC 200 to GNU 9.7. In LXC 700, `stat --version` reports `stat (uutils coreutils) 0.10.0`.

## Scope and acceptance
1. Keep GNU and BusyBox detection unchanged.
2. If `stat --help` matches neither, check `stat --version` via the existing read-only host command runner; accept uutils **only** when it explicitly identifies `uutils coreutils`.
3. Continue exercising the full existing syntax contract for **every** host command; never whitelist uutils without checking feature support. Keep the tool paths and no-mutation guarantees.
4. Extend the capability model's accepted profile to `uutils` and tests for all three profile types, unsupported tools/contracts, and version fallback.
5. Verify the Go unit suite on Go 1.27.1 and conduct a realistic container/host capability test on LXC 700 (native uutils). No extra runtime dependency.
6. Confirm upstream CI publishes `ghcr.io/godsquantum/syncbridge:latest` from main; deploy only after tests, retain rollback and existing /config.
7. Verify LXC 200 SyncBridge job ID 4 `0 2 * * *`, the SSD->HDD guard and rsync + Syncthing notification; protect Pegasus offline and all sources.

## Deliberate non-goals
No new protocol, storage mount, Docker bind, privilege extension, rewrite to Rust, new UI framework, or hidden migration of existing jobs. Use existing Go 1.27, Svelte 5 and native Linux tools. Existing Paperless job path failures are independent.

## References reviewed 14 October 2026
- Local repository `GodsQuantum/SyncBridge` at `464f82b`, existing Go 1.27.1, Svelte 5, fsnotify 1.10.1, robfig/cron 3.0.1.
- `https://github.com/uutils/coreutils` — compatibility is a goal, not guaranteed; semantics must be probed.
- `https://github.com/DietrichGebert/ponytail` — smallest secure complete diff.
- `https://github.com/multica-ai/andrej-karpathy-skills` — test-driven, no speculative refactor.
- Superpowers test-driven-development, systematic-debugging and verification-before-completion.

## Security update (14 October 2026)
GitHub CI's govulncheck detected vulnerable Go 1.27.1 standard-library paths.
Go 1.27.2 (released 8 October 2026) patches these advisories.
Pinned the official multiarchitecture Go 1.27.2 Alpine 3.24 image digest, CI matrix,
module toolchain, and deployment contract test; no vulnerability suppression.
