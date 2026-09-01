package catalog

import (
	"context"
	"fmt"
)

func (c *Catalog) initialize(ctx context.Context) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = FULL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA journal_mode = WAL",
	}
	for _, statement := range pragmas {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure SQLite with %q: %w", statement, err)
		}
	}

	if err := c.initializeSchema(ctx); err != nil {
		return err
	}
	return c.Ping(ctx)
}

func (c *Catalog) initializeSchema(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog schema initialization: %w", err)
	}
	defer tx.Rollback()

	const schema = `
CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    protocol TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mailboxes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    path_key TEXT NOT NULL,
    delimiter TEXT NOT NULL DEFAULT '',
    uid_validity INTEGER NOT NULL CHECK (uid_validity >= 0),
    last_uid INTEGER NOT NULL DEFAULT 0 CHECK (last_uid >= 0),
    remote_messages INTEGER NOT NULL DEFAULT 0 CHECK (remote_messages >= 0),
    active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    created_at TEXT NOT NULL,
    last_seen_at TEXT,
    UNIQUE(account_id, name, uid_validity)
);
CREATE INDEX IF NOT EXISTS mailboxes_account_active ON mailboxes(account_id, active, name);
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    mailbox_id INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    uid INTEGER NOT NULL CHECK (uid > 0),
    internal_date TEXT,
    size INTEGER NOT NULL CHECK (size >= 0),
    sha256 TEXT NOT NULL,
    relative_path TEXT NOT NULL UNIQUE,
    sidecar_path TEXT NOT NULL UNIQUE,
    flags_json TEXT NOT NULL DEFAULT '[]',
    subject TEXT NOT NULL DEFAULT '',
    from_value TEXT NOT NULL DEFAULT '',
    to_value TEXT NOT NULL DEFAULT '',
    header_message_id TEXT NOT NULL DEFAULT '',
    header_date TEXT,
    parse_error TEXT NOT NULL DEFAULT '',
    archived_at TEXT NOT NULL,
    last_verified_at TEXT,
    verify_error TEXT NOT NULL DEFAULT '',
    UNIQUE(mailbox_id, uid)
);
CREATE INDEX IF NOT EXISTS messages_mailbox_uid ON messages(mailbox_id, uid);
CREATE INDEX IF NOT EXISTS messages_archived_at ON messages(archived_at);
CREATE TABLE IF NOT EXISTS collections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('mail', 'contact', 'calendar')),
    name TEXT NOT NULL,
    remote_id TEXT NOT NULL,
    remote_url TEXT NOT NULL DEFAULT '',
    sync_token TEXT NOT NULL DEFAULT '',
    active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    created_at TEXT NOT NULL,
    last_seen_at TEXT,
    UNIQUE(account_id, kind, remote_id)
);
CREATE INDEX IF NOT EXISTS collections_account_active ON collections(account_id, active, kind, name);
CREATE TABLE IF NOT EXISTS objects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    remote_id TEXT NOT NULL,
    etag TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    sha256 TEXT NOT NULL,
    relative_path TEXT NOT NULL UNIQUE,
    sidecar_path TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL DEFAULT '',
    flags_json TEXT NOT NULL DEFAULT '[]',
    internal_date TEXT,
    remote_collections_json TEXT NOT NULL DEFAULT '[]',
    archived_at TEXT NOT NULL,
    last_verified_at TEXT,
    verify_error TEXT NOT NULL DEFAULT '',
    UNIQUE(collection_id, remote_id)
);
CREATE INDEX IF NOT EXISTS objects_collection_remote ON objects(collection_id, remote_id);
CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    operation TEXT NOT NULL CHECK (operation IN ('backup', 'verify', 'restore')),
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled', 'interrupted')),
    requested_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    error TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS runs_requested_at ON runs(requested_at DESC);
`
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply catalog schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit catalog schema: %w", err)
	}
	return nil
}
