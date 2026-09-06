# Backup

Two one-shot backup programs:

- **`pimbackup`** archives IMAP or JMAP mail and CardDAV or CalDAV objects in plain, protocol-aware files. It can restore those files through the original protocol.
- **`appbackup`** snapshots application files and consistent PostgreSQL, MySQL, MariaDB, or SQLite dumps into an encrypted local Restic repository.

There is no daemon, scheduler, listening socket, or embedded cloud-provider stack. Run the programs from systemd, cron, a container job, or a Kubernetes CronJob.

## Build

Go 1.27 or newer is required.

```sh
go build -trimpath -o bin/ ./cmd/...
go test ./...
```

`appbackup` also needs `restic`. Server database backups need the corresponding native client (`pg_dump`/`pg_restore`, `mysqldump`, or `mariadb-dump`). SQLite support is built in. `pimbackup` has no runtime helper dependency.

## Quick start

Create and edit a strict JSON configuration. `config init` refuses to overwrite an existing file:

```sh
pimbackup config init --output /etc/pimbackup/config.json
pimbackup config validate --config /etc/pimbackup/config.json
pimbackup backup --config /etc/pimbackup/config.json --data-dir /var/lib/pimbackup

appbackup config init --output /etc/appbackup/config.json
appbackup config validate --config /etc/appbackup/config.json
appbackup backup --config /etc/appbackup/config.json --data-dir /var/lib/appbackup
```

The defaults are `/etc/pimbackup/config.json`, `/etc/appbackup/config.json`, and `/data`. `--config`, `--data-dir`, `--json`, `--log-level`, and `--log-format` are global and may appear before or after the command. Human-readable output is the default; `--json` emits machine-readable records.

Examples are in [`configs/`](configs/). `config show` prints the effective configuration with secrets redacted.

### Secrets

Prefer mounted secret files over literal JSON or environment values:

```json
{
  "password_file": "/run/secrets/pimbackup_password"
}
```

Restic accepts `APPBACKUP_RESTIC_PASSWORD_FILE`. PIM account secrets can be supplied as `PIMBACKUP_ACCOUNT_<ID>_PASSWORD_FILE` or `PIMBACKUP_ACCOUNT_<ID>_TOKEN_FILE`, where `<ID>` is uppercased and non-alphanumeric characters become underscores.

Grant each source credential the least privilege needed:

- PIM backup accounts need read access; use separate restore credentials when practical.
- Database users need only the permissions required by their native dump client.
- Cloud-mirror credentials must be provider-enforced read-only credentials. The direction of an `rclone sync` command is not an access-control boundary.

## Commands

Both programs expose:

```text
backup   status   list   show   verify   repair
config init|validate|show   check   version   help
```

`pimbackup restore` imports selected archived messages or objects to an explicitly named remote target and requires `--confirm`.

`appbackup export` extracts a recovery point to a local staging directory and requires `--confirm`. It never writes to an application or database. Inspect the export and perform the production restore with the native application or database tooling.

Useful examples:

```sh
pimbackup status
pimbackup list messages --account personal --mailbox INBOX --limit 50
pimbackup show message --id 42
pimbackup verify
pimbackup restore --message-id 42 --target-account recovery \
  --target-mailbox Restored --confirm

appbackup status
appbackup list recovery-points --application website
appbackup show recovery-point --id 0195...
appbackup list contents --id 0195... --limit 100
appbackup verify                         # newest successful point per app
appbackup verify --application website  # newest successful point for website
appbackup verify --id 0195...           # one point
appbackup export --id 0195... --confirm
```

`verify` is intentionally deep. Application verification runs one full `restic check` per invocation, confirms each selected snapshot is readable, restores dumps into staging, and runs configured native dump checks. PIM verification streams and validates every selected canonical file. Schedule it less often than `backup` if the data set is large.

`check` is a quick operational diagnostic. `repair --confirm` runs a full catalog and filesystem reconciliation. Normal startup only scans everything for a new catalog or after detecting an interrupted operation.

## Cloud data

Cloud Backup was removed. Mirror remote data to a local directory with the maintained `rclone` executable, then include that directory in an `appbackup` recovery point. This keeps cloud-provider code outside the backup process while Restic supplies encryption, deduplication, history, and integrity checking.

Run the mirror as a separate job and identity before App Backup. [`configs/cloud-mirror.appbackup.example.json`](configs/cloud-mirror.appbackup.example.json) only names the finished local mirror. [`cloud-mirror.service`](deploy/systemd/cloud-mirror.service) and the supplied App Backup drop-in order the jobs. The Compose file requires a successful mirror container before App Backup starts. App Backup never receives the cloud credential.

## Scheduling and containers

Hardened systemd service/timer examples are in [`deploy/systemd/`](deploy/systemd/). They run as a dedicated unprivileged `backup` user, use systemd credentials, expose no port, drop capabilities, and make the host filesystem read-only except for declared state paths.

The multi-stage [`deploy/container/Containerfile`](deploy/container/Containerfile) uses digest-pinned Docker Hardened Images from `dhi.io` and has separate targets for each runtime:

```sh
docker build --target pimbackup -f deploy/container/Containerfile -t pimbackup .
docker build --target appbackup -f deploy/container/Containerfile -t appbackup .
docker build --target rclone -f deploy/container/Containerfile -t backup-rclone .
docker build --target appbackup-postgresql -f deploy/container/Containerfile -t appbackup-postgresql .
docker build --target appbackup-mysql -f deploy/container/Containerfile -t appbackup-mysql .
docker build --target appbackup-mariadb -f deploy/container/Containerfile -t appbackup-mariadb .
```

The base App Backup target handles files and embedded SQLite. Choose a database target when native client tools are needed. Run the rclone target separately so cloud and Restic credentials never share a container.

Docker does not read `.gitignore`. [`deploy/container/Containerfile.dockerignore`](deploy/container/Containerfile.dockerignore) is a deny-by-default build-context allowlist. Podman and Buildah use [`.containerignore`](.containerignore) with the same rules.

Runtime containers use UID/GID `65532`, need a writable `/data`, and should receive configurations and secrets through read-only mounts. Do not publish ports. A Compose job example is in [`deploy/container/compose.yaml`](deploy/container/compose.yaml). Create bind-mounted state directories with ownership `65532:65532` before running it.

## Storage and recovery guarantees

- State directories and catalogs are owner-only; manifests and sidecars use atomic write-and-rename.
- Catalogs use SQLite WAL, full synchronous writes, foreign keys, and one connection.
- Canonical payload paths are derived from validated identifiers and confined with `os.Root` where supported by the store.
- A per-data-directory process lock prevents overlapping mutating jobs.
- Restic repositories always live at `<appbackup-data-dir>/restic`; source and staging paths may not overlap the repository.
- PIM archives and application exports are plaintext. Use encrypted persistent storage (for example LUKS, FileVault, or an encrypted managed volume), protect filesystem snapshots, and replicate recovery data to a separate failure domain.
- Keep the data directory unavailable to unrelated workloads. Root or the account that owns the files can still read secrets and plaintext data.

See [`SPEC.md`](SPEC.md) for formats, command contracts, configuration fields, and failure semantics.

## Migration from the former daemon suite

Existing configurations need these changes:

1. Stop the old services and back up every data directory. Never point two versions at one directory concurrently.
2. Delete `server`, `schedule`, `engine`, and App Backup `restic.repository` fields. Unknown fields are rejected. If the old Restic repository was not `<data_dir>/restic`, move or copy the complete repository there while no backup is running.
3. Export any old Cloud Backup versions that must be retained. The new commands do not import the former Cloud catalog. Seed a read-only remote-to-local rclone mirror, then include it in an App Backup application.
4. Replace Application Backup `restore` calls with `export --confirm`; restore from the exported files using native tools.
5. Move scheduling to a system timer or job controller.
6. Run `repair` once after moving an existing PIM or Application data directory. Their payloads and manifests remain readable; the catalog is derived state.
