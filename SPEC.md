# Backup suite specification

## 1. Scope

The repository builds two commands.

| Command | Purpose | Canonical payload |
| --- | --- | --- |
| `pimbackup` | Back up and restore IMAP, JMAP mail, CardDAV, and CalDAV | RFC 5322 mail, vCard, iCalendar, JSON sidecars |
| `appbackup` | Back up files and native database dumps | Encrypted Restic snapshots and JSON manifests |

Both commands run one requested operation and exit. They do not listen on a network port or schedule their own work. Cloud-provider support belongs in a separately maintained `rclone` process, not in either Go binary.

## 2. Common command contract

Global options may precede or follow a command:

```text
--config PATH
--data-dir PATH
--json
--log-level debug|info|warn|error
--log-format json|text
```

Configuration precedence is defaults, JSON, environment, then CLI. JSON decoding rejects unknown fields, duplicate fields, trailing values, files over 4 MiB, and non-regular files. `data_dir` must be a clean absolute path and cannot be the filesystem root.

Commands return zero only after the requested operation succeeds. Configuration, lock, protocol, verification, and partial-backup errors return nonzero. SIGINT and SIGTERM cancel the operation. Cleanup that must release a quiesced application runs with cancellation detached.

Each data directory has a process lock. Mutating operations fail rather than queue when another process owns it. The catalog records queued, running, succeeded, failed, and interrupted runs.

Human-readable output goes to stdout by default. `--json` emits JSON. Logs go to stderr and never carry raw configured secrets. Persisted operation errors pass through the same redactor.

## 3. PIM Backup

### 3.1 Commands

```text
pimbackup backup [--account ID ...]
pimbackup status
pimbackup list accounts
pimbackup list mailboxes [--account ID]
pimbackup list messages [--account ID] [--mailbox NAME] [--limit N] [--offset N]
pimbackup list collections [--account ID] [--kind mail|contact|calendar]
pimbackup list objects [--account ID] [--collection NAME] [--kind KIND] [--limit N] [--offset N]
pimbackup list runs [--limit N] [--offset N]
pimbackup show message --id N
pimbackup show object --id N
pimbackup show run --id UUID
pimbackup verify [--account ID] [--message-id N | --object-id N]
pimbackup restore (--message-id N ... | --object-id N ...) --target-account ID \
  [--target-mailbox NAME | --target-collection NAME] --confirm
pimbackup repair --confirm
pimbackup config init [--output PATH]
pimbackup config validate
pimbackup config show
pimbackup check
pimbackup version
```

Restore rejects a mixed message/object request, an empty selection, a missing target, or a request without `--confirm`. It imports new remote objects. It does not overwrite or delete an existing remote object. IMAP restore strips server-managed and destructive flags.

### 3.2 Configuration

Top-level fields are `data_dir`, `log`, and `accounts`.

Common account fields:

| Field | Meaning |
| --- | --- |
| `id` | Stable lowercase identifier, at most 63 characters |
| `protocol` | `imap`, `jmap`, `carddav`, or `caldav`; default `imap` |
| `url` | Explicit HTTPS session or DAV URL |
| `host`, `port` | IMAP endpoint, or host used for JMAP/DAV discovery |
| `username` | Login name and optional discovery source |
| `auth` | `basic` or `bearer` for HTTP protocols |
| `password_file`, `token_file` | Preferred secret sources |
| `ca_file` | PEM file containing private trust roots |
| `timeout` | Per-network-operation duration |
| `disabled` | Excludes the account from backup and check |

IMAP adds `tls` with `implicit`, `starttls`, or `plain`; `mailboxes`; and `exclude_mailboxes`. HTTP protocols use `collections` and `exclude_collections`. An empty include list means `*`.

Plain HTTP, IMAP without TLS, and `insecure_skip_verify` require `allow_insecure: true`. This acknowledgement exists for isolated tests. Production configurations should use TLS and `ca_file` for private PKI.

Supported environment settings are:

```text
PIMBACKUP_CONFIG
PIMBACKUP_DATA_DIR
PIMBACKUP_LOG_LEVEL
PIMBACKUP_LOG_FORMAT
PIMBACKUP_ACCOUNT_<ID>_PASSWORD
PIMBACKUP_ACCOUNT_<ID>_PASSWORD_FILE
PIMBACKUP_ACCOUNT_<ID>_TOKEN
PIMBACKUP_ACCOUNT_<ID>_TOKEN_FILE
```

Setting both the direct and file form of one secret is an error, even if one value is empty.

### 3.3 Backup protocol

IMAP backup performs these steps for each selected mailbox:

1. Select the mailbox and read `UIDVALIDITY` and `UIDNEXT`.
2. Create or find the canonical mailbox generation.
3. Search for UIDs above the last committed UID.
4. Fetch messages in sorted batches of at most 10 with `BODY.PEEK`.
5. Stream each message through SHA-256 hashing, write its payload and sidecar atomically, and record parsed header metadata.
6. Commit completed batch rows to the catalog in UID order.
7. Advance the mailbox cursor only after the full mailbox pass succeeds.

A changed `UIDVALIDITY` creates a new generation. Old generations remain recoverable.

JMAP and DAV backup first gathers a stable object list or change set. It checks matching ETags against the local payload before downloading. Up to four objects download concurrently. Each worker validates and atomically commits its object before the collection sync token advances. A failed worker leaves the token unchanged, so the next run retries the change set and skips valid files already committed.

JMAP authenticated requests may target only origins advertised by the authenticated session. An authenticated redirect to any other origin fails before transport. DAV requests and redirects remain on the configured DAV origin. This prevents bearer or Basic credentials from following attacker-controlled URLs.

### 3.4 Canonical storage

The data directory contains:

```text
pim.db
.pimbackup.lock
accounts/<account>/mail/<mailbox-key>/uidvalidity-<n>/
  mailbox.json
  <uid>.eml
  <uid>.json
accounts/<account>/mail-jmap/<collection-hash>/
accounts/<account>/contacts/<collection-hash>/
accounts/<account>/calendars/<collection-hash>/
  collection.json
  <object-hash>.eml|vcf|ics
  <object-hash>.json
```

Mailbox keys contain a readable slug and a truncated SHA-256 digest. Object and collection names derive from SHA-256 digests, not remote path text. Sidecars include remote identity, timestamps, sizes, content types, flags, and SHA-256 values. Payloads remain useful without SQLite.

Basic checks compare size and SHA-256. Deep verification also parses MIME, vCard, or iCalendar syntax. A vCard must have `VERSION` and `FN`; an iCalendar file must have `VERSION`.

## 4. Application Backup

### 4.1 Commands

```text
appbackup backup [--application ID ...]
appbackup status
appbackup list applications
appbackup list recovery-points [--application ID] [--limit N] [--offset N]
appbackup list contents --id RECOVERY_POINT [--limit N] [--offset N]
appbackup list runs [--limit N] [--offset N]
appbackup show recovery-point --id RECOVERY_POINT
appbackup show run --id UUID
appbackup verify [--id RECOVERY_POINT | --application ID]
appbackup export --id RECOVERY_POINT --confirm
appbackup repair --confirm
appbackup config init [--output PATH]
appbackup config validate
appbackup config show
appbackup check
appbackup version
```

Without a selector, `verify` chooses the newest successful recovery point for each application. It invokes `restic check` once, then validates each chosen snapshot and dump. `export` writes to `<data_dir>/exports/<run-id>/<recovery-point-id>` and reports that path. It never executes a database restore or copies files over a configured source.

### 4.2 Configuration

Top-level fields are `data_dir`, `restic`, `log`, and `applications`.

`restic` accepts `binary`, `password_file` or `password`, and `timeout`. The repository path is fixed at `<data_dir>/restic`. The loader also accepts:

```text
APPBACKUP_CONFIG
APPBACKUP_DATA_DIR
APPBACKUP_LOG_LEVEL
APPBACKUP_LOG_FORMAT
APPBACKUP_RESTIC_BINARY
APPBACKUP_RESTIC_TIMEOUT
APPBACKUP_RESTIC_PASSWORD
APPBACKUP_RESTIC_PASSWORD_FILE
```

Each application has:

| Field | Meaning |
| --- | --- |
| `id` | Stable lowercase identifier, at most 63 characters |
| `paths` | Clean absolute source paths outside `data_dir` |
| `databases` | Native database dump definitions |
| `hooks` | `pre_backup`, `quiesce`, `unquiesce`, and `post_backup` command arrays |
| `timeout` | Whole-application timeout, default 4 hours |
| `verify_after_backup` | Run targeted verification after the snapshot |
| `disabled` | Exclude the application |

Hook entries contain `binary`, literal `args`, and `timeout`. The program calls binaries directly without a shell. Newlines and NULs are rejected. Hook subprocesses do not inherit Restic or database password variables known to App Backup. App Backup also strips common ambient AWS, Azure, Google, Backblaze, and `RCLONE_CONFIG_*` credentials. Supply a narrowly scoped credential through an explicit hook argument or mounted file.

Database fields are `id`, `type`, `binary`, `restore_binary`, `host`, `port`, `user`, `name`, `path`, `password_file` or `password`, `verify_command`, and `timeout`. Types and defaults:

| Type | Dump | Default check |
| --- | --- | --- |
| `postgresql` | `pg_dump --format=custom` | `pg_restore --list`; reports `unknown` without a full test restore |
| `mysql` | `mysqldump --single-transaction --routines --events` | hash and size only; reports `unknown` |
| `mariadb` | `mariadb-dump --single-transaction --routines --events` | hash and size only; reports `unknown` |
| `sqlite` | embedded read-only `VACUUM INTO` snapshot | embedded `PRAGMA quick_check`; reports `passed` |

A `verify_command` replaces the default dump check. Every `{dump}` substring in its arguments expands to the staged dump path. Configure a disposable database restore if a `passed` result is required for PostgreSQL, MySQL, or MariaDB.

### 4.3 Backup transaction

For each selected application, App Backup:

1. Writes and catalogs a `running` recovery-point manifest.
2. Runs `pre_backup` hooks.
3. Runs `quiesce` hooks.
4. Creates native database dumps in a private staging directory.
5. snapshots configured paths and staging in one Restic invocation.
6. Runs `unquiesce` if quiescing started, even after cancellation or failure.
7. Runs `post_backup`, also after failure.
8. Atomically commits the final manifest and catalog row.
9. Removes staging.
10. If configured, lists the exact snapshot, restores each dump into verification staging, checks hashes and native format, and records the result.

A failure marks that application's point failed and continues with the next selected application. The command returns nonzero if any selected application failed. Restic tags each snapshot with `appbackup`, `application:<id>`, and `recovery-point:<id>`.

### 4.4 Storage

```text
app.db
.appbackup.lock
restic/
recovery-points/<id>/manifest.json
staging/<operation-or-point-id>/
exports/<run-id>/<recovery-point-id>/
```

Directories use mode `0700`; regular metadata and dump files use `0600`. The store rejects symlinked control directories and confines metadata operations beneath an `os.Root`. Restic initialization refuses a non-regular or symlinked repository config file. Backup source paths and restore targets cannot overlap the repository.

The JSON recovery-point manifest is authoritative. It records source paths, dump hashes, tool versions, hook results, component results, the Restic snapshot ID, status, errors, and verification. SQLite is a query index.

## 5. Crash recovery and repair

Catalog writes use SQLite WAL, `synchronous=FULL`, foreign keys, a 5-second busy timeout, and one database connection. Payload and manifest writes use a temporary file, `fsync`, rename, and parent-directory `fsync`.

At startup, each program marks queued or running catalog operations as interrupted. A new catalog or any interrupted operation triggers a full scan of canonical files. Healthy startup skips that scan. `repair` always performs the full scan and rebuilds missing derived rows.

Application reconciliation also finds `running` recovery-point manifests. It replays missing `unquiesce` and `post_backup` hooks, marks the point interrupted, and catalogs it. Hook success markers prevent replay of steps already committed to the manifest.

Deleting a catalog does not delete canonical payloads. The next open recreates and rebuilds it. Corrupt canonical payloads are not silently repaired from catalog data. PIM Backup refetches a damaged remote object when the remote still has it; otherwise verification reports the damage.

## 6. Security and deployment

- Run one dedicated, unprivileged OS identity per trust domain.
- Keep configurations and secret files read-only to that identity. Prefer short-lived or provider-scoped credentials.
- Mount application sources read-only. Only the App Backup data directory and an optional cloud mirror need write access.
- Do not expose a port. There is no HTTP API.
- Use normal certificate verification. Add private roots with `ca_file`; do not use TLS bypass in production.
- Encrypt the volume that contains PIM payloads, staging, exports, catalogs, and Restic credentials. Restic encrypts its repository, but not the other paths.
- Replicate recovery data to another machine or storage account after the one-shot job completes. Do not copy a live Restic repository while App Backup is writing it.
- Test PIM restore into a separate mailbox or collection. Test application exports with native restore tools before an incident.

The supplied systemd units drop all capabilities and grant write access only to declared state directories. Container builds use digest-pinned `dhi.io` Docker Hardened Images and run as UID/GID 65532.
