# Deployment notes

## systemd

Create a locked service account and state directories:

```sh
sudo groupadd --system backup
sudo useradd --system --gid backup --home-dir /var/lib/backup --shell /usr/sbin/nologin backup
sudo useradd --system --gid backup --home-dir /var/lib/cloud-mirror --shell /usr/sbin/nologin cloudmirror
sudo install -d -o root -g backup -m 0750 /etc/backup /etc/backup/credentials
sudo install -d -o backup -g backup -m 0700 /var/lib/pimbackup /var/lib/appbackup
sudo install -d -o cloudmirror -g backup -m 0750 /var/lib/cloud-mirror
sudo install -o root -g root -m 0755 pimbackup appbackup rclone /usr/local/bin/
sudo install -o root -g backup -m 0640 deploy/systemd/pimbackup.json /etc/backup/pimbackup.json
sudo install -o root -g backup -m 0640 deploy/systemd/appbackup.json /etc/backup/appbackup.json
sudo install -o root -g root -m 0644 deploy/systemd/*.service deploy/systemd/*.timer /etc/systemd/system/
sudo install -d -o root -g root -m 0755 /etc/systemd/system/appbackup.service.d
sudo install -o root -g root -m 0644 deploy/systemd/appbackup.service.d/cloud-mirror.conf /etc/systemd/system/appbackup.service.d/
```

Place JSON configuration in `/etc/backup`. Put source credentials in `/etc/backup/credentials` with mode `0600`; root owns them. The units use `LoadCredential`, so the service sees read-only copies under `/run/credentials/<unit>/`.

The PIM unit maps `pim-password` to the `personal` account through `PIMBACKUP_ACCOUNT_PERSONAL_PASSWORD_FILE`. Change the environment variable when the account ID differs. The App unit maps `restic-password` through `APPBACKUP_RESTIC_PASSWORD_FILE`. The cloud mirror runs as `cloudmirror` and can read only its rclone credential and mirror directory. Add one `LoadCredential` line for each database secret and reference its runtime path in JSON. For example:

```json
"password_file": "/run/credentials/appbackup.service/postgres-password"
```

Install `restic`, `rclone`, and any configured database clients in `/usr/local/bin` or another root-owned directory in `PATH`. Remove the cloud mirror drop-in and service if no application uses it.

Check the sandbox and run each service once before enabling timers:

```sh
sudo systemd-analyze security pimbackup.service cloud-mirror.service appbackup.service
sudo systemctl daemon-reload
sudo systemctl start pimbackup.service
sudo systemctl start appbackup.service
sudo journalctl -u pimbackup.service -u cloud-mirror.service -u appbackup.service
sudo systemctl enable --now pimbackup.timer appbackup.timer
systemctl list-timers '*backup*'
```

`ProtectSystem=strict` makes source paths read-only. Add only required mirror or staging paths to `ReadWritePaths`. Do not make the application source writable to the backup account.

## Containers

The Containerfile uses Docker Hardened Images. Authenticate to `dhi.io` before building if your Docker account requires it:

```sh
docker login dhi.io
docker build --target pimbackup -f deploy/container/Containerfile -t pimbackup:local .
docker build --target appbackup-postgresql -f deploy/container/Containerfile -t appbackup:local .
docker build --target rclone -f deploy/container/Containerfile -t backup-rclone:local .
```

The plain `appbackup` target includes Restic and embedded SQLite support. Use `appbackup-postgresql`, `appbackup-mysql`, or `appbackup-mariadb` when the job needs that native client. The separate `rclone` target keeps cloud credentials out of App Backup.

Docker does not inspect `.gitignore` when it builds a context. `deploy/container/Containerfile.dockerignore` therefore repeats a narrow allowlist for Docker. Podman and Buildah read the root `.containerignore`. Both files ignore everything except the Go module, Go source, and Containerfile inputs.

Base image tags are pinned to multi-platform index digests. Update a tag and its digest together, review the DHI provenance and vulnerability report, then rebuild all targets. Tool source versions are pinned by `RESTIC_VERSION` and `RCLONE_VERSION` build arguments.

Every final image runs as UID/GID 65532. Before using the Compose example:

```sh
install -d -m 0700 deploy/container/data/{pim,app,cloud-mirror}
install -d -m 0750 deploy/container/source
sudo chown -R 65532:65532 deploy/container/data
install -d -m 0700 deploy/container/secrets
```

Adapt the JSON files so secret paths use `/run/secrets/<name>`. Run jobs from the host scheduler rather than keeping a container alive:

```sh
docker compose -f deploy/container/compose.yaml run --rm pimbackup
docker compose -f deploy/container/compose.yaml run --rm appbackup
```

Use `read_only: true`, drop all capabilities, keep `no-new-privileges`, and mount source data with `:ro`. Neither image needs a published port.

## Cloud mirror

Run rclone first under a separate identity:

```text
rclone sync remote:path /writable/local-mirror --config /run/secrets/rclone_config --check-first
appbackup backup --config /etc/backup/appbackup.json
```

The systemd drop-in makes `appbackup.service` require a successful `cloud-mirror.service`. Compose uses `condition: service_completed_successfully`. The remote is always the source. Give rclone a provider policy that denies upload, overwrite, and delete operations on the remote. Use a credential that does not require rclone to rewrite its read-only config file. App Backup receives read-only access to the finished mirror and no cloud credential. Restic retains prior versions when a remote file changes or disappears.

Do not place the mirror inside the App Backup data directory. Do not put the Restic repository inside the mirror.

## Persistent storage

Restic encrypts `<data_dir>/restic`. PIM files, catalogs, transient database dumps, and application exports remain plaintext. Put both data directories on encrypted storage and encrypt host-level snapshots. Keep at least one tested replica in another failure domain.

Stop the timer and wait for the one-shot service before copying a whole data directory. Copying SQLite WAL files or a Restic repository during a write can produce an inconsistent replica.
