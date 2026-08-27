# SyncBridge — documentation française

SyncBridge est un petit plan de contrôle auto-hébergé qui exécute des commandes, scripts et synchronisations **dans les namespaces de l'hôte Linux**, tout en restant lui-même dans un conteneur Docker durci unique.

## Fonctions disponibles

- commandes libres, scripts hôte et jobs de synchronisation rsync/rclone ;
- exécution hôte via `nsenter`, sans socket Docker ;
- identité hôte explicite : UID/GID fixe ou propriétaire du script lorsque disponible ;
- déclenchement manuel, cron SyncBridge ou surveillance de dossier ;
- planification persistante possédée par l’hôte : `/etc/cron.d` ou unités systemd `.service/.path` utilisant le même wrapper validé ;
- inventaire/import cron, systemd et inotify, avec toggle, suppression réversible et restauration après revalidation serveur ;
- détection de commandes rsync/rclone dans les chemins hôte configurés par `SYNCBRIDGE_IMPORT_PATHS` ;
- surveillance événementielle, polling ou hybride avec filtres, anti-rebond et budget de scan ;
- réservation anti-chevauchement atomique, timeout et arrêt TERM→KILL du groupe de processus ;
- logs bornés et historique terminal persistant ;
- API v1 avec révisions/ETag et événements SSE reprenables ;
- gestion d'instances SyncBridge distantes joignables ;
- image durcie sans bind mounts larges de `/home`, `/mnt`, `/etc`, `/proc` ni `docker.sock`.

## Déploiement

Le modèle public unique se trouve dans [`../deploy/compose.yaml`](../deploy/compose.yaml). Copie [`../deploy/syncbridge.env.example`](../deploy/syncbridge.env.example) vers `.env`, puis adapte uniquement le port, le dossier de configuration, le fuseau horaire et le propriétaire UID/GID des fichiers persistés.

```bash
mkdir -p /opt/syncbridge
docker compose --env-file .env -f compose.yaml up -d
```

L'interface écoute par défaut sur `8787`. Le premier compte enregistré devient administrateur.

Le conteneur utilise `pid: host` et `CAP_SYS_ADMIN` pour entrer dans les namespaces de l'hôte. Le navigateur de dossiers et les watchers utilisent la vue `/proc/1/root`; il n'est donc pas nécessaire de monter `/home`, `/mnt` ou `/proc` dans le conteneur.

## Modèle de sécurité

SyncBridge est un contrôleur d'exécution hôte : un administrateur de SyncBridge dispose par conception d'une capacité d'exécution de niveau hôte. Il faut donc le placer derrière une politique d'accès de confiance (VPN/reverse proxy authentifié + TLS), et non l'exposer directement à Internet.

Le Compose public applique notamment :

- `user: 0:0` uniquement pour permettre l'entrée dans les namespaces ;
- `pid: host` ;
- `SYS_ADMIN` ;
- `no-new-privileges` ;
- rootfs du conteneur en lecture seule ;
- seulement `/config` en volume persistant ;
- tmpfs dédiés pour `/tmp` et `/run` ;
- aucun `privileged: true` ni socket Docker.

Voir [`deployment.md`](deployment.md) et [`architecture.md`](architecture.md).

## API v1

- `GET|POST /api/v1/jobs`
- `GET|PUT|DELETE /api/v1/jobs/{id}`
- `GET|POST /api/v1/jobs/{id}/runs`
- `GET /api/v1/runs/{runID}`
- `POST /api/v1/runs/{runID}/stop`
- `GET /api/v1/events`
- `GET /api/v1/capabilities`

Les mutations de jobs utilisent les révisions persistées/ETag. Les routes de compatibilité utilisées par l'UI délèguent aux mêmes services vNext : il n'y a plus de seconde map d'exécution parallèle.

## Développement

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/syncbridge
node --check cmd/syncbridge/web/app.js
```
