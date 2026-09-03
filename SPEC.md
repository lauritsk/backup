# Backup suite specification

Status: All three binaries implement their common CLI, catalog, scheduler, HTTP API, locking, diagnostics, backup, browse, verification, and restore entry points. PIM Backup supports IMAP, JMAP Mail, CardDAV, and CalDAV, with incremental JMAP and DAV synchronization. Cloud Backup inventories remotes and streams each object through an atomic local commit. Application Backup creates Restic recovery points with native database dumps and staged restore exports. The current limits are documented in each tool section. Container images and deployment files are not yet checked in.

This file is the product and implementation specification for the suite.

## Suite boundary

The repository produces three independent programs:

| Program | Job | Required external engine |
| --- | --- | --- |
| `pimbackup` | Back up and restore mail, contacts, and calendars | Protocol libraries for IMAP, JMAP, CardDAV, and CalDAV |
| `cloudbackup` | Acquire files from remote storage | None. It embeds rclone's Go packages. |
| `appbackup` | Create logical application recovery points | Restic, `pg_dump` plus `pg_restore` for PostgreSQL, or one MySQL/MariaDB dump client. Hook and verification commands remain optional. |

Each program has its own process, configuration, catalog, scheduler, API, and `/data` mount. No program depends on another program at build time or runtime. There is no suite daemon and no coordinator. Each program is intended to have its own runtime image, but image packaging is still pending.

Backup payloads and application-owned state stay beneath `/data`. Temporary files must also live beneath `/data` so that atomic renames do not cross filesystems. An explicit restore may write to a target selected for that restore. Cloud Backup is the exception: it never writes to an rclone remote and materializes restores beneath `/data/restores` for the operator to retrieve.

The tools do not manage storage snapshots, replication, off-site copies, filesystem compression, deduplication, or storage-level retention.

## Repository layout

The checked-in code follows this shape:

```text
.
├── cmd/
│   ├── appbackup/          # appbackup process entry point
│   ├── cloudbackup/        # cloudbackup process entry point
│   └── pimbackup/          # pimbackup process entry point
├── configs/                # example declarative configuration
├── internal/
│   ├── appbackup/
│   │   ├── catalog/        # Application SQLite schema and queries
│   │   ├── config/         # Application configuration
│   │   ├── database/       # embedded SQLite and native server database tools
│   │   ├── engine/         # Docker and Podman socket diagnostics
│   │   ├── hooks/          # lifecycle command process boundary
│   │   ├── model/          # Application request and response records
│   │   └── restic/         # Restic process boundary
│   ├── atomicfile/         # durable temporary write, fsync, and rename
│   ├── buildinfo/          # shared build metadata
│   ├── cloudbackup/
│   │   ├── catalog/        # Cloud SQLite schema and queries
│   │   ├── config/         # Cloud configuration
│   │   ├── model/          # Cloud request and response records
│   │   └── rclone/         # read-only embedded rclone adapter
│   ├── configutil/         # strict JSON, duration, and environment parsing
│   ├── httpapi/            # bounded JSON requests, responses, and pagination
│   ├── logging/            # shared slog construction
│   ├── operationexecutor/  # shared run transitions, cancellation, and panic recovery
│   ├── operationlock/      # process-local and filesystem operation lock
│   ├── pimbackup/
│   │   ├── catalog/        # PIM SQLite schema and queries
│   │   ├── config/         # PIM configuration
│   │   ├── dav/            # CardDAV and CalDAV network adapter
│   │   ├── imap/           # IMAP network adapter
│   │   ├── jmap/           # JMAP Mail network adapter
│   │   ├── mailstore/      # canonical IMAP .eml files and sidecars
│   │   ├── objectstore/    # JMAP .eml, vCard, and iCalendar files
│   │   └── model/          # PIM request and response records
│   ├── run/                # shared operation and status vocabulary
│   ├── safeerror/          # bounded single-line error text
│   └── secret/             # shared direct value and *_FILE resolution
├── SPEC.md
└── go.mod
```

Add packages when they get working code. Do not create an empty package tree to predict every adapter. Cloud Backup and Application Backup will add private packages under their matching tool directory.

Package names may change when implementation exposes a better boundary. The import rules may not:

1. A `cmd/<tool>` package imports only its matching tool package and small shared packages needed to start the process.
2. A tool package may import shared packages.
3. Shared packages never import a tool package.
4. Tool packages never import one another.
5. Deployment files contain no backup, verification, or restore logic.

## What belongs in shared code

Code is shared only when at least two tools need the same behavior and the failure semantics are the same. Similar names are not enough. PIM mailbox synchronization and a streamed cloud acquisition are both called backup, but their data state machines are unrelated.

The first PIM implementation may keep code local until a second tool needs it. Extracting a small proven function is cheaper than maintaining a generic framework built from guesses.

| Area | Shared responsibility | Tool-owned responsibility | State |
| --- | --- | --- | --- |
| Build metadata | Version, revision, build time, Go version, CLI and API representation | Image labels and tool name | `internal/buildinfo` exists |
| Secrets | Reject direct plus `_FILE`, read the file, remove one final line ending, never log values | Which fields are secret and how credentials are used | `internal/secret` exists |
| Run vocabulary | `backup`, `verify`, and `restore` operations; queued, running, terminal, and interrupted states | Run detail, resource identifiers, progress, and catalog persistence | `internal/run` exists |
| Configuration loading | Strict bounded JSON, duplicate detection, duration parsing, and environment booleans | Config structs, defaults, environment prefix, semantic validation, and redaction | Shared mechanics are in `internal/configutil`; schemas remain tool-owned |
| Logging | Construct `slog` and choose level and format | Domain event names and safe fields | Shared construction is in `internal/logging` |
| Process lifecycle | Run transitions, signal cancellation, bounded catalog writes and shutdown, and panic recovery | Startup cleanup and closing protocol sessions and external processes | Run execution is shared in `internal/operationexecutor`; startup cleanup remains tool-owned |
| HTTP mechanics | Strict bounded JSON bodies, JSON responses, and pagination | Authentication, routes, service errors, and domain request bodies | Shared mechanics are in `internal/httpapi`; handlers remain tool-owned |
| Health and diagnostics | Aggregate named checks and stable statuses | IMAP login, rclone remote, Restic repository, database, and engine checks | Implemented by all three tools |
| Scheduling | Fixed intervals, cancellation, no overlapping scheduled run | Which configured accounts, sources, or applications a tick selects | Implemented separately by all three tools |
| Operation locking | Process-local exclusion and a lock beneath `/data` for cron versus server races | Lock filename, conflict wording, and whether a domain read can coexist with an operation | Shared gate is in `internal/operationlock` |
| Durable files | Same-filesystem temporary files, file sync, atomic rename, parent directory sync, and rooted atomic writes | Canonical names, payload validation, and reconciliation | All three tools use shared atomic writes for owned metadata and exports; Cloud also uses them for acquired payloads |
| Safe errors | Remove line breaks and bound persisted or returned error text | Choosing which failures are safe to expose | Shared mechanics are in `internal/safeerror` |
| Verification flow | Start and finish a run, cancellation, panic recovery, and bounded final catalog writes | Every integrity check and test restore | Common execution is in `internal/operationexecutor`; reports remain tool-owned |
| SQLite mechanics | Connection settings and transaction helpers only if more than one tool needs identical behavior | Schema, queries, cursors, and reconciliation | Each tool keeps its own catalog and schema |

The following must not become shared suite abstractions:

- A universal source, resource, adapter, or backup-engine interface.
- A database schema used by all three tools.
- A common canonical payload format.
- A wrapper that hides IMAP, rclone, Restic, SQL dump tools, or container engines behind one API.
- Cross-tool account, source, or recovery-point identifiers.
- A plugin system.
- A central scheduler or API.

The root command trees also stay in each tool while their options are unsettled. Help text, subcommands, and restore arguments will differ enough that a home-grown CLI framework would save little.

## Common runtime contract

### Commands

Every program will expose these top-level operations:

```text
serve
backup
browse
verify
restore
config validate
config show
check
version
```

`pimbackup` also has `db rebuild`. A tool may add domain commands, but it may not change the meaning of the common four operations.

- `backup` acquires data and records a run.
- `browse` reads already backed-up data. Browse requests are not persisted as runs by default.
- `verify` checks recoverability using domain knowledge and records a run.
- `restore` recovers a selected item or recovery point and records a run.

Cron, systemd timers, and `serve` call the same application service used by the direct command. No command shells out to its own `serve` process.

### Configuration

Each tool owns one JSON configuration schema and one environment prefix:

```text
PIMBACKUP_*
CLOUDBACKUP_*
APPBACKUP_*
```

Precedence is fixed:

```text
defaults -> JSON -> environment -> CLI
```

JSON is the normal home for accounts, source lists, application definitions, resource selectors, and hook lists. Environment variables and flags should remain small scalar overrides. Secret-bearing scalar settings support both a direct value and a sibling `_FILE` setting. Setting both is an error even if one value is empty.

`config validate` decodes and validates local syntax and semantics without DNS, network calls, external command execution, or opening a container socket. `check` performs those operational tests. `config show` displays the effective value after precedence and replaces every secret with a fixed redaction marker. The HTTP API does not mutate configuration.

### Runs and concurrency

A recorded operation follows this state sequence:

```text
queued -> running -> succeeded
                  -> failed
                  -> canceled
                  -> interrupted
```

On startup, a tool changes any run left in `running` to `interrupted`, then performs its domain reconciliation before accepting another operation. Error fields must be safe to show through the API. Raw command output, protocol frames, URLs containing credentials, and secret values do not belong in run records.

Only one backup, verification, or restore that can touch the same data set may run at once. The exclusion must work between two processes sharing `/data`, not only between goroutines. A failed lock attempt returns a clear conflict instead of waiting forever. Restore is never started by the interval scheduler.

### HTTP API

Each `serve` command hosts its own API. Common endpoints are:

```text
GET  /healthz
GET  /readyz
GET  /api/v1/version
GET  /api/v1/runs
GET  /api/v1/runs/{id}
POST /api/v1/backup
POST /api/v1/verify
POST /api/v1/restore
```

A successful trigger returns `202 Accepted` and a run record. Tool packages own the request body and domain routes for accounts, resources, sources, applications, recovery points, and browse results. APIs use bounded pagination and stream large payloads rather than loading them into memory.

Liveness reports whether the process can serve requests. Readiness checks configuration, the `/data` mount, catalog initialization, and required local executables. It does not require every remote service to be reachable on every probe. Remote access belongs in `check` and operation results.

The server defaults to a loopback listener. Every tool supports an optional bearer token and rejects an unauthenticated non-loopback listener unless configuration explicitly allows it. Health and readiness endpoints remain unauthenticated. A deployment that exposes an API must still provide TLS termination and network policy, and may use an authentication proxy instead of the built-in token.

### Filesystem rules

Application-owned backup writes use this order when the payload is a normal file:

```text
create temp beneath /data
write and validate
fsync temp
rename to final path
fsync parent directory
commit catalog metadata
advance remote state
```

A tool must tolerate a crash between any two steps. On the next run it reconciles temporary files, final files, catalog rows, and remote cursors conservatively. It never advances a cursor based on an uncommitted or non-durable payload.

Restic owns durability inside its repository. Cloud Backup uses the embedded rclone listing and object-read APIs to inventory each source and stream changed objects into application-owned temporary files. It checks the inventory size and SHA-256 when the remote provides one, syncs and atomically promotes each file, and records that durable file before moving to the next object. It writes the historical manifest after all objects commit, then updates `latest.json`. A crash can leave newer per-file catalog rows without a completed manifest; startup does not overwrite those rows with an older manifest, and the next backup completes the source manifest.

Paths derived from remote names require traversal checks and deterministic collision handling. Symlinks beneath writable data paths must not let a process escape `/data`. Cloud payload and restore access, PIM standard-object access, and Application-owned manifest access use descriptor-relative `os.Root` operations. The IMAP store and paths handed to SQLite, Restic, the embedded rclone backends, and native database processes still need path-based APIs; their components are validated before use. All persistent formats need a documented recovery path using normal tools.

## PIM Backup custom design

PIM Backup understands protocol state and standard personal-information formats. This code is not useful to the other two programs and stays under `internal/pimbackup`.

### PIM-owned logic

- Account and resource configuration for IMAP, JMAP, CardDAV, and CalDAV.
- Authentication and TLS settings for each protocol.
- Protocol-specific discovery, pagination where available, timeouts, and server quirks.
- IMAP mailbox discovery and the identity tuple `mailbox + UIDVALIDITY + UID`.
- JMAP `Email/changes` state, stable initial query snapshots, opaque identities, and explicit supported object models.
- Canonical mail, contact, and calendar naming.
- SQLite tables for accounts, resources, remote IDs, sync state, metadata, runs, and errors.
- Filesystem and catalog reconciliation, including `db rebuild`.
- Domain browse responses and payload streaming.
- Verification of MIME, vCard, iCalendar, catalog links, and remote identity metadata.
- Restore behavior such as IMAP APPEND, JMAP creation, or export of standards files.

### PIM endpoint discovery

JMAP, CardDAV, and CalDAV account URLs are optional. Without one, PIM Backup derives the domain from `host` or an email-style username. JMAP uses `https://<domain>/.well-known/jmap`. DAV uses RFC 6764 SRV/TXT discovery through go-webdav and falls back to `/.well-known/carddav` or `/.well-known/caldav`, then discovers the current principal and collection home. An explicit URL overrides discovery; startup logs which mode each account uses without logging the URL.

### PIM data

The initial layout is:

```text
/data/
├── pim.db
└── accounts/
    └── <account-id>/
        ├── mail/           # IMAP mailbox generations
        ├── mail-jmap/      # JMAP Mail collections
        ├── contacts/
        └── calendars/
```

Mail payloads are unchanged RFC822/MIME `.eml` files. Contacts use vCard and calendars use iCalendar unless a protocol requires additional documented metadata. Small JSON sidecars are allowed for server metadata that the standard payload cannot carry, such as IMAP flags or internal dates. Sidecars are documented, versioned, and optional for reading the main payload. They are not an archive container.

SQLite is an operational catalog, not the only copy of backed-up data. A missing database must not make `.eml`, `.vcf`, or `.ics` files unreadable. `db rebuild` recovers records from file paths, standards files, and sidecars. It may reset remote cursors and perform a conservative remote reconciliation.

### IMAP adapter

The IMAP adapter:

1. Discover selected mailboxes and record UIDVALIDITY.
2. Fetch complete raw messages without normalizing headers or MIME bodies.
3. Commit each message with the durable write order above.
4. Advance the highest safe UID only after all earlier required messages are durable and cataloged.
5. Keep local messages when the server expunges them unless the operator runs a future explicit prune operation.
6. Treat a UIDVALIDITY change as a new identity set. It must not overwrite the old set based only on UID reuse.
7. Resume after cancellation or process death without duplicating or losing payloads.
8. Browse mailbox metadata and stream the original `.eml` file.
9. Verify file readability, recorded size or digest, MIME parsing, identity metadata, and catalog links.
10. Restore selected messages while preserving raw content and recording any server-assigned identity.

JMAP Mail uses the standard-object store because JMAP identities are opaque strings rather than IMAP UIDs; it still preserves unchanged RFC822 payloads and exposes them through the same browse, verify, and restore operations. Contacts and calendars have their own collection and object models rather than pretending every object is mail-shaped.

JMAP builds and confirms a stable `Email/query` snapshot on the first backup, stores the `Email/get` state, and uses paginated `Email/changes` calls after that. It fetches created and updated messages and retains archived messages that the server reports as destroyed. If the server can no longer calculate changes from an old state, it rebuilds the stable snapshot. CardDAV and CalDAV use the RFC 6578 `sync-collection` report and commit the returned token only after all changed objects are durable and cataloged. DAV servers that reject the report, and servers that reject an expired token, fall back to full enumeration without advancing the rejected token.

## Cloud Backup custom design

Cloud Backup is an rclone acquisition service, not a second rclone configuration language and not a remote replication service. Its rclone configuration contains only `config_path`; there is no executable path.

### Cloud-owned logic

- Source configuration and mapping to an existing rclone configuration.
- Read-only use of rclone filesystem, listing, hashing, and object-open APIs.
- Source access checks, filters, include and exclude rules, transfer concurrency, and bandwidth policy.
- Mapping remote paths to local source roots.
- Acquisition manifests, source metadata, run detail, and any local catalog.
- Browse and export behavior for acquired files.
- Verification based on local size and SHA-256 values recorded after acquisition.
- Diagnostics for the embedded rclone adapter, configuration, authentication, and source access.

A source is always the remote side of an rclone operation and the destination is always a path beneath `/data`. The adapter exposes no upload, move, delete, purge, or sync method. Restore materializes files beneath `/data/restores/<run-id>` and reports their paths. Re-upload is an operator action outside this program.

The expected layout is:

```text
/data/
├── cloud.db
├── sources/
│   └── <source-id>/
│       ├── files/
│       └── manifests/
└── restores/
```

The acquisition operation uses rclone's `operations.ListJSON` against the configured remote, validates and de-duplicates the returned paths, and opens each object that needs a local commit through `fs.Object.Open`. The adapter has no remote-write method. A file deleted at the source remains local by default and remains in later manifests. Historical versions come from snapshots of `/data`, not duplicate trees invented by Cloud Backup.

Cloud Backup computes SHA-256 while each object streams and classifies the committed file as added or changed. It skips a file only when the remote supplies a matching SHA-256, the size matches, and the rooted local file is readable. Remotes without SHA-256 support are read again so the local digest never relies on size and modification time alone. A remote read failure identifies the object path in the run detail, leaves the prior canonical file in place, and prevents a source manifest from being committed. Verification recomputes local hashes and checks cataloged files. Comparing with the live source remains a separate diagnostic because source equality does not prove that a historical backup is readable. The `unknown` count in the report model is reserved and is currently always zero.

The intended Cloud Backup image contains the Go binary and CA certificates. It does not need an rclone executable. The binary registers all rclone backends, which increases its dependency and binary size. No image definition is currently checked in.

## Application Backup custom design

Application Backup coordinates a declared recovery point. Restic owns filesystem and volume storage. Native clients own logical database dump and restore formats.

### Application-owned logic

- Application definitions, component order, and recovery-point manifests.
- Pre-backup, quiesce, unquiesce, and post-backup hooks.
- Selection of files and mounted volumes for Restic.
- PostgreSQL, MariaDB or MySQL, and SQLite dump commands, plus restore-side validation utilities.
- Restic repository initialization, snapshot tagging, lookup, restore, and integrity commands.
- Optional Docker or Podman diagnostics over a Unix socket and operator-defined container verification commands.
- Recording dump-client versions and running operator-supplied verification commands.
- Recovery ordering and partial-failure reporting.
- Browse results that join recovery-point manifests and Restic snapshot contents.

The expected layout is:

```text
/data/
├── app.db
├── restic/
├── recovery-points/
│   └── <recovery-point-id>/
│       └── manifest.json
├── staging/
└── restores/
```

A recovery point follows this order:

```text
acquire operation lock
run pre-backup and quiesce hooks
create native database dumps beneath /data/staging
ask Restic to snapshot declared files, volumes, and dumps into /data/restic
write and sync the recovery-point manifest
unquiesce and run post-backup hooks
run configured verification
record final status
```

Cleanup hooks run after a failure whenever their precondition ran. The manifest records tool versions, component results, Restic snapshot IDs, dump metadata, hook outcomes, and the latest verification report. It never records command environments or secret arguments.

A dump is not verified merely because it exists. SQLite verification runs `PRAGMA quick_check` through `modernc.org/sqlite`. PostgreSQL archive listing checks use `pg_restore`. MariaDB or MySQL dump hashes remain `unknown` unless the database has an operator-supplied `verify_command` that restores into a compatible instance and runs integrity checks. Application Backup records the dump client version but does not infer server compatibility; the operator owns compatibility inside `verify_command`. Container-based verification is opt-in through that command and still requires the selected Docker or Podman executable. Engine diagnostics call `/_ping` and `/version` directly over the configured Unix socket. The diagnostics command warns that socket access is equivalent to broad host privilege.

SQLite backup opens the live database through `modernc.org/sqlite` and creates a consistent snapshot with `VACUUM INTO`. It does not require or configure a SQLite executable. The database `binary` field applies only to PostgreSQL, MySQL, and MariaDB. The `restore_binary` field applies only to PostgreSQL.

Restores currently materialize an entire Restic snapshot beneath `/data/restores/<run-id>/<recovery-point-id>`. Application Backup never performs an implicit in-place restore. Direct restore into a database or declared volume is a future explicit mode; it will require confirmation, preflight checks, component selection, and no scheduler entry point.

The intended Application Backup image is custom per supported client set. File-only and SQLite backups need Restic. MySQL and MariaDB add one dump client. PostgreSQL adds `pg_dump` and `pg_restore`. Docker and Podman clients are needed only when an operator chooses one for `verify_command`. No image definitions are currently checked in.

## Ownership summary

| Concern | PIM Backup | Cloud Backup | Application Backup |
| --- | --- | --- | --- |
| Remote access | IMAP, JMAP, CardDAV, CalDAV clients | Embedded rclone read APIs | Native server database tools, Restic, and direct Docker or Podman socket diagnostics |
| Durable payload | `.eml`, `.vcf`, `.ics`, documented sidecars | Normal files and manifests | Restic repository, native dumps, recovery manifests |
| Incremental state | IMAP UIDs and UIDVALIDITY; JMAP Email states; DAV sync tokens and ETags | Remote inventories, per-file durable commits, and acquisition manifests | Restic snapshot IDs and component completion |
| Catalog | PIM resources and protocol identities | Sources, files, and acquisitions | Applications and recovery-point browse fields |
| Browse | Mailboxes, messages, contacts, calendars | Source trees and file metadata | Recovery points, components, and Restic contents |
| Verify | Parse standards files and reconcile identities | Rehash files and compare manifests | Restic checks and test-restored databases |
| Restore | Protocol-aware append or standards export | Local export only | Staged recovery-point export only |
| Runtime image | Go binary and trust data | Go binary and trust data | Go binary plus Restic and selected server database clients |

## Test boundaries

Shared packages need table-driven unit tests for precedence, redaction, secret ambiguity, status handling, lock contention, cancellation, HTTP errors, and durable file behavior.

Release tests should use fakes at mature process or protocol boundaries, not at a universal backup interface:

- PIM tests should use protocol test servers and fixture standards files. Crash tests should stop execution after each durability step and then run reconciliation.
- Cloud tests should exercise the embedded adapter against a local rclone backend and assert that its interface exposes no remote-write operation.
- Application tests should use fake Restic and database executables for failure ordering. Container-backed tests remain separate and opt-in.

Every restore path needs an automated round trip before its backup path is called complete. The current suite has IMAP, JMAP, CardDAV, Cloud export, and staged Application service round trips. The embedded rclone adapter also has a local-backend acquisition test. CalDAV has adapter-level backup and restore coverage but still needs an end-to-end service round trip. Per-durability-step crash injection and opt-in container verification tests are still pending.

## Implementation order

1. Keep the three binaries buildable and independent.
2. Add configuration, logging, lifecycle, locking, run persistence, and durable-file code only as the IMAP slice needs them.
3. Keep IMAP, JMAP Mail, CardDAV, and CalDAV backup, browse, verify, restore, reconciliation, and `db rebuild` covered by round-trip tests.
4. Keep JMAP Mail's incremental backup, browse, verification, and restore paths at parity with or ahead of the IMAP path.
5. Keep Cloud Backup's embedded rclone adapter read-only on the remote side.
6. Keep Application Backup's Restic, server database, and hook process boundaries separate. Keep engine diagnostics in the Unix-socket HTTP adapter.
7. Extract more shared code only after the second real use proves identical behavior.

PIM Backup currently uses go-imap v2, go-jmap, go-webdav, gofrs flock, and modernc SQLite. Before the first published release, each catalog creates its current schema directly. All three HTTP APIs use an optional bearer token and refuse an unauthenticated non-loopback listener unless configuration explicitly allows it.
