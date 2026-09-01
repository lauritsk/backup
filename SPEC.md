# Backup suite specification

Status: PIM Backup implements IMAP and JMAP Mail, CardDAV contacts, and CalDAV calendars. Cloud Backup implements rclone acquisition, local browse, verification, restore export, diagnostics, scheduling, and its HTTP API. Application Backup remains a command scaffold.

This file is the product and implementation specification for the suite.

## Suite boundary

The repository produces three independent programs:

| Program | Job | Required external engine |
| --- | --- | --- |
| `pimbackup` | Back up and restore mail, contacts, and calendars | Protocol libraries for IMAP, JMAP, CardDAV, and CalDAV |
| `cloudbackup` | Acquire files from remote storage | `rclone` |
| `appbackup` | Create logical application recovery points | Restic, database clients, and optional Docker or Podman clients |

Each program has its own process, configuration, catalog, scheduler, API, image, and `/data` mount. No program depends on another program at build time or runtime. There is no suite daemon and no coordinator.

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
│   ├── appbackup/          # Application Backup command scaffold
│   ├── atomicfile/         # durable temporary write, fsync, and rename
│   ├── buildinfo/          # shared build metadata
│   ├── cloudbackup/        # Cloud Backup service and rclone process boundary
│   ├── configutil/         # strict JSON, duration, and environment parsing
│   ├── logging/            # shared slog construction
│   ├── operationlock/      # process-local and filesystem operation lock
│   ├── pimbackup/
│   │   ├── catalog/        # PIM SQLite schema and queries
│   │   ├── config/         # PIM JSON, environment, CLI, and secrets
│   │   ├── dav/            # CardDAV and CalDAV network adapter
│   │   ├── imap/           # IMAP network adapter
│   │   ├── jmap/           # JMAP Mail network adapter
│   │   ├── mailstore/      # canonical IMAP .eml files and sidecars
│   │   ├── objectstore/    # JMAP .eml, vCard, and iCalendar files
│   │   └── model/          # PIM request, response, and catalog records
│   ├── run/                # shared operation and status vocabulary
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

Code is shared only when at least two tools need the same behavior and the failure semantics are the same. Similar names are not enough. PIM mailbox synchronization and an rclone copy are both called backup, but their state machines are unrelated.

The first PIM implementation may keep code local until a second tool needs it. Extracting a small proven function is cheaper than maintaining a generic framework built from guesses.

| Area | Shared responsibility | Tool-owned responsibility | State |
| --- | --- | --- | --- |
| Build metadata | Version, revision, build time, Go version, CLI and API representation | Image labels and tool name | `internal/buildinfo` exists |
| Secrets | Reject direct plus `_FILE`, read the file, remove one final line ending, never log values | Which fields are secret and how credentials are used | `internal/secret` exists |
| Run vocabulary | `backup`, `verify`, and `restore` operations; queued, running, terminal, and interrupted states | Run detail, resource identifiers, progress, and catalog persistence | `internal/run` exists |
| Configuration loading | Strict bounded JSON, duplicate detection, duration parsing, and environment booleans | Config structs, defaults, environment prefix, semantic validation, and redaction | Shared mechanics are in `internal/configutil`; schemas remain tool-owned |
| Logging | Construct `slog` and choose level and format | Domain event names and safe fields | Shared construction is in `internal/logging` |
| Process lifecycle | Signal cancellation, bounded shutdown, and startup cleanup | Closing protocol sessions and external processes | Implemented separately for PIM and Cloud |
| HTTP mechanics | Timeouts, body limits, JSON errors, common health/version/run routes | Resource, browse, backup, verification, and restore request bodies | Implemented separately for PIM and Cloud |
| Health and diagnostics | Aggregate named checks and stable statuses | IMAP login, rclone remote, Restic repository, database, and engine checks | Implemented for PIM and Cloud |
| Scheduling | Fixed intervals, cancellation, no overlapping scheduled run | Which configured accounts, sources, or applications a tick selects | Implemented separately for PIM and Cloud |
| Operation locking | Process-local exclusion and a lock beneath `/data` for cron versus server races | Lock filename, conflict wording, and whether a domain read can coexist with an operation | Shared gate is in `internal/operationlock` |
| Durable files | Same-filesystem temporary files, file sync, atomic rename, and parent directory sync | Canonical names, payload validation, and reconciliation | Shared atomic writes are used by PIM and Cloud; payload rules remain tool-owned |
| Verification flow | Start and finish a run, cancellation, report envelope, safe error recording | Every integrity check and test restore | Implemented separately for PIM and Cloud |
| SQLite mechanics | Connection settings and transaction helpers only if more than one tool needs identical behavior | Schema, queries, cursors, and reconciliation | PIM and Cloud keep separate catalogs and schemas |

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

The server defaults to a loopback listener. A deployment that exposes it must provide the network policy, authentication proxy, and TLS termination until the suite has a shared, reviewed authentication design. Health endpoints may be exposed separately by deployment configuration.

### Filesystem rules

Backup writes use this order when the payload is a normal file:

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

Paths derived from remote names require traversal checks and deterministic collision handling. Symlinks beneath writable data paths must not let a process escape `/data`. All persistent formats need a documented recovery path using normal tools.

## PIM Backup custom design

PIM Backup understands protocol state and standard personal-information formats. This code is not useful to the other two programs and stays under `internal/pimbackup`.

### PIM-owned logic

- Account and resource configuration for IMAP, JMAP, CardDAV, and CalDAV.
- Authentication and TLS settings for each protocol.
- Capability negotiation, pagination, rate limits, retries, and server quirks.
- IMAP mailbox discovery and the identity tuple `mailbox + UIDVALIDITY + UID`.
- JMAP state tokens and explicit supported object models.
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
        ├── mail/
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

JMAP Mail uses the standard-object store because JMAP identities are opaque strings rather than IMAP UIDs; it still preserves unchanged RFC822 payloads and exposes them through the same browse, verify, and restore operations. JMAP state handling remains in its adapter. Contacts and calendars have their own collection and object models rather than pretending every object is mail-shaped.

## Cloud Backup custom design

Cloud Backup is an rclone acquisition service, not a second rclone configuration language and not a remote replication service.

### Cloud-owned logic

- Source configuration and mapping to an existing rclone configuration.
- Safe construction of rclone arguments and environment variables.
- Source capability discovery, filters, include and exclude rules, and bandwidth policy.
- Mapping remote paths to local source roots.
- Acquisition manifests, source metadata, run detail, and any local catalog.
- Browse and export behavior for acquired files.
- Verification based on locally recorded size and hashes, with explicit `unknown` results where a remote cannot provide a trustworthy hash.
- Diagnostics for the rclone binary, configuration, authentication, and source access.

A source is always the remote side of an rclone operation and the destination is always a path beneath `/data`. The tool does not invoke rclone commands that write, move, delete, purge, or sync to a remote. Restore materializes files beneath `/data/restores/<run-id>` and reports their paths. Re-upload is an operator action outside this program.

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

The acquisition operation behaves like a non-destructive copy. A file deleted at the source remains local by default. A changed source file replaces the current local file through an atomic write. Historical versions come from snapshots of `/data`, not duplicate trees invented by Cloud Backup. Each completed run records enough metadata to explain what rclone added, changed, skipped, or could not read.

Verification recomputes local hashes where available and checks the manifest against the file tree. Comparing with the live source is a separate diagnostic because source equality does not prove that a historical backup is readable.

The Cloud Backup image is custom. It includes a pinned rclone binary and CA certificates but does not inherit database or Restic clients from Application Backup.

## Application Backup custom design

Application Backup coordinates a declared recovery point. Restic owns filesystem and volume storage. Native clients own logical database dump and restore formats.

### Application-owned logic

- Application definitions, component order, and recovery-point manifests.
- Pre-backup, quiesce, unquiesce, and post-backup hooks.
- Selection of files and mounted volumes for Restic.
- PostgreSQL, MariaDB or MySQL, and SQLite dump and restore commands.
- Restic repository initialization, snapshot tagging, lookup, restore, and integrity commands.
- Optional Docker or Podman inspection and short-lived verification containers.
- Matching a dump with a compatible verification server version.
- Recovery ordering and partial-failure reporting.
- Browse results that join manifests, Restic snapshots, dumps, and verification reports.

The expected layout is:

```text
/data/
├── app.db
├── restic/
├── recovery-points/
│   └── <recovery-point-id>/
│       ├── manifest.json
│       └── verification.json
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

Cleanup hooks run after a failure whenever their precondition ran. The manifest records tool versions, component results, Restic snapshot IDs, dump metadata, hook outcomes, and verification status. It never records command environments or secret arguments.

A dump is not verified merely because it exists. PostgreSQL and MariaDB or MySQL verification should restore into an ephemeral compatible instance, then run connection and basic integrity checks. Container-based verification is opt-in and requires an explicitly configured Docker or Podman socket. The diagnostics command must warn that access to an engine socket is equivalent to broad host privilege. Native installations may use local clients or an operator-supplied verification command.

SQLite backup uses SQLite's supported backup mechanisms or a native utility, not a copy of a live database file unless the application is stopped and configuration says the copy is safe.

Restores first materialize files beneath `/data/restores/<run-id>`. Direct restore into a database or declared volume is a separate explicit mode with confirmation, preflight checks, and no scheduler entry point. Application Backup never performs an implicit in-place restore.

The Application Backup image is custom per supported client set. It includes pinned Restic and database clients. A minimal PIM image and an rclone image should not carry these tools.

## Ownership summary

| Concern | PIM Backup | Cloud Backup | Application Backup |
| --- | --- | --- | --- |
| Remote access | IMAP, JMAP, CardDAV, CalDAV clients | rclone read operations | Database, Restic, Docker, and Podman clients |
| Durable payload | `.eml`, `.vcf`, `.ics`, documented sidecars | Normal files and manifests | Restic repository, native dumps, recovery manifests |
| Incremental state | UIDs, UIDVALIDITY, JMAP states, ETags, sync tokens | rclone listings and acquisition manifests | Restic snapshot IDs and component completion |
| Catalog | PIM resources and protocol identities | Sources, files, and acquisitions | Applications, components, and recovery points |
| Browse | Mailboxes, messages, contacts, calendars | Source trees and file metadata | Recovery points, components, and Restic contents |
| Verify | Parse standards files and reconcile identities | Rehash files and compare manifests | Restic checks and test-restored databases |
| Restore | Protocol-aware append or standards export | Local export only | Staged or explicit component restore |
| Runtime image | Go binary and trust data | Go binary plus rclone | Go binary plus Restic and selected native clients |

## Test boundaries

Shared packages need table-driven unit tests for precedence, redaction, secret ambiguity, status handling, lock contention, cancellation, HTTP errors, and durable file behavior.

Tool tests use fakes at mature process or protocol boundaries, not at a universal backup interface:

- PIM tests use an IMAP test server and fixture `.eml` files. Crash tests stop execution after each durability step and then run reconciliation.
- Cloud tests place a fake `rclone` executable on `PATH`, capture arguments, and assert that no command can write to a remote. Integration tests use a local rclone backend.
- Application tests use fake Restic and database executables for failure ordering. Container-backed tests are separate and opt-in.

Every restore path needs an automated round trip before its backup path is called complete.

## Implementation order

1. Keep the three binaries buildable and independent.
2. Add configuration, logging, lifecycle, locking, run persistence, and durable-file code only as the IMAP slice needs them.
3. Keep IMAP, JMAP Mail, CardDAV, and CalDAV backup, browse, verify, restore, reconciliation, and `db rebuild` covered by round-trip tests.
4. Keep Cloud Backup's rclone process boundary read-only on the remote side.
5. Build Application Backup around Restic and native database clients.
6. Extract more shared code only after the second real use proves identical behavior.

PIM Backup currently uses go-imap v2, go-jmap, go-webdav, gofrs flock, and modernc SQLite. Before the first published release, the catalog creates the current schema directly. The HTTP API uses an optional bearer token and refuses an unauthenticated non-loopback listener unless configuration explicitly allows it.
