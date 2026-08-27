# Deployment

## Public Compose contract

SyncBridge ships one generic Compose example: [`../deploy/compose.yaml`](../deploy/compose.yaml). Machine-specific names, networks, paths, IP addresses, and credentials belong in the operator's private deployment configuration, not in this repository.

Required container properties:

| Setting | Purpose |
| --- | --- |
| `user: "0:0"` | allows namespace operations while persisted files are explicitly owned by `SB_UID:SB_GID` |
| `pid: host` | exposes the host PID namespace and PID 1 |
| `cap_add: SYS_ADMIN` | permits `nsenter` into the host namespaces |
| `cap_add: SYS_PTRACE` | permits the ptrace-gated `/proc/1/root` host filesystem view |
| `read_only: true` | prevents writes to the image filesystem |
| `no-new-privileges:true` | blocks privilege escalation through execve |
| `/config` bind | only persistent application state mount |
| `/tmp`, `/run` tmpfs | bounded writable runtime scratch space |

Do **not** add the Docker socket, host `/proc`, host `/etc`, `/home`, or `/mnt` merely to execute jobs. Host commands run through `nsenter`; host path browsing and watches resolve through `/proc/1/root` when `pid: host` is enabled.

## Environment

Copy `deploy/syncbridge.env.example` to a private `.env` file.

- `SYNCBRIDGE_TAG` — image tag, default `latest`.
- `SYNCBRIDGE_HOST_PORT` — published HTTP port, default `8787`.
- `SYNCBRIDGE_CONFIG_DIR` — host path for persistent `/config` state.
- `SYNCBRIDGE_FILE_UID` / `SYNCBRIDGE_FILE_GID` — owner of files SyncBridge persists.
- `TZ` — container timezone.
- `SYNCBRIDGE_IMPORT_PATHS` — optional colon-separated logical host paths scanned for rsync/rclone commands.

`SB_UID` and `SB_GID` are **not** the identity used to execute every job. Each job has its own explicit host identity policy.

## Host prerequisites

SyncBridge probes host tools through the namespace boundary. Install the tools needed by your jobs on the host itself. For example, a sync job using rsync needs host `rsync`; an rclone job needs host `rclone`.

## Start and inspect

```bash
docker compose --env-file .env -f deploy/compose.yaml pull
docker compose --env-file .env -f deploy/compose.yaml up -d
docker compose --env-file .env -f deploy/compose.yaml ps
docker compose --env-file .env -f deploy/compose.yaml logs --tail=100 syncbridge
```

The image has a built-in healthcheck. CI also verifies that the hardened container can read the host filesystem view and execute a trivial command through `nsenter` before an image is published.

## Networking

The public example intentionally declares no project-specific external network. Add your own private reverse-proxy network in your local Compose override if required. Keep machine names, internal domains, LAN addresses, and credentials outside the public repository.

## Import scanning

`SYNCBRIDGE_IMPORT_PATHS` is an optional colon-separated list of logical host paths to scan for simple rsync/rclone commands. Paths are resolved through `/proc/1/root`; do not add matching bind mounts. When unset, SyncBridge scans common cron locations only.

Persistent host scheduling also uses `/proc/1/root` for atomic cron/systemd file operations and `nsenter` for host `systemctl`.
