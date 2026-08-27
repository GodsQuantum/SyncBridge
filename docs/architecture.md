# Architecture

## Scope

SyncBridge separates **where work executes** from **who owns the trigger**.

All jobs execute in the Linux host namespaces. Triggers can be manual, SyncBridge-owned cron/watch, or host-owned persistent cron/systemd artifacts. Persistent host scheduling invokes the same validated host wrapper used by interactive runs.

## Main components

- **JobRepository** — schema-v2 jobs, immutable snapshots, revisions, migration, atomic persistence.
- **RunService** — atomic run reservation, overlap policy, state machine, bounded logs/events/history, cancellation.
- **PlanCompiler** — validates an immutable job snapshot and compiles a host execution plan.
- **WrapperStore** — installs the host-side wrapper used for a specific immutable plan.
- **HostExecutor** — starts a direct `nsenter` argv, manages the host process group, captures logs, and applies TERM→KILL stop behavior.
- **Scheduler** — owns only SyncBridge cron trigger registration.
- **WatchManager** — owns fsnotify/poll/hybrid trigger lifecycle and never executes processes itself.
- **PersistentScheduler** — reconciles host-owned `/etc/cron.d` or systemd `.service/.path` artifacts against enabled jobs.
- **SystemService** — inventories and safely mutates host cron/systemd/inotify items after server-side revalidation.
- **CapabilityService** — probes host tools and namespace capabilities.
- **App/API** — owns component wiring, API v1, compatibility adapters, and graceful shutdown.

## Host namespace boundary

The controller container runs with `pid: host`, `SYS_ADMIN`, and `SYS_PTRACE`, but not `privileged: true`. `SYS_ADMIN` permits namespace entry; `SYS_PTRACE` satisfies the kernel access check required to dereference `/proc/1/root` for PID 1. On AppArmor hosts it runs with `apparmor=unconfined`, because the standard `docker-default` profile intentionally denies cross-profile host access. Docker's default seccomp profile and `no-new-privileges` remain enabled. This makes the SyncBridge admin boundary a host-administration boundary by design, not a container-isolation boundary.

Process execution:

```text
RunService → PlanCompiler → WrapperStore → HostExecutor
                                      └→ nsenter → host command/script/sync
```

Filesystem view:

```text
logical host path /srv/data
          │
          ▼
/proc/1/root/srv/data
          │
          ├── folder browser
          └── WatchManager fsnotify / polling
```

Persisted jobs and APIs always retain the logical host path. `/proc/1/root` is an internal implementation detail used to avoid broad host bind mounts.

## Run invariants

- a run ID is reserved before asynchronous execution begins;
- `overlap=skip` cannot race two starts into execution;
- queue-latest retains at most one pending replacement;
- accepted runs outlive the originating HTTP request context;
- logs and retained terminal runs are bounded;
- history persistence is serialized without keeping the main run-state mutex across disk I/O;
- timeout/stop target the host process group, first TERM and then KILL after grace.

## Watch invariants

- the parent directory is watched only to detect root recreation;
- sibling events outside the logical source do not trigger a job;
- event and polling paths apply the same glob semantics;
- polling is context-cancellable and has an entry budget;
- watcher lifecycle serializes reconcile/close operations and prevents stale revision callbacks from starting work.

## HTTP surface

API v1 is the durable control-plane contract. The current UI still uses compatibility routes for some operations, but those are adapters over `JobRepository` and `RunService`, not a second source of truth.

System inventory/import compatibility routes are backed by `SystemService`; they resolve logical host paths through `/proc/1/root` and execute manager commands through `HostCommandRunner`. Persistent scheduler artifacts are reconciled by `PersistentScheduler`; no legacy host bind mounts or second execution engine are used.

## Future boundaries

Future work should continue moving compatibility UI calls onto API v1 while preserving the same repository, run-service, host namespace, and server-side revalidation boundaries.
