package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lauritsk/backup/internal/pimbackup/model"
)

func (c *Catalog) EnsureCollection(ctx context.Context, collection model.Collection) (model.Collection, error) {
	now := time.Now().UTC()
	_, err := c.db.ExecContext(ctx, `
INSERT INTO collections (account_id, kind, name, remote_id, remote_url, sync_token, active, created_at, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
ON CONFLICT(account_id, kind, remote_id) DO UPDATE SET
    name = excluded.name, remote_url = excluded.remote_url, active = 1, last_seen_at = excluded.last_seen_at
`, collection.AccountID, collection.Kind, collection.Name, collection.RemoteID, collection.RemoteURL,
		collection.SyncToken, formatTime(now), formatTime(now))
	if err != nil {
		return model.Collection{}, fmt.Errorf("upsert collection %q: %w", collection.Name, err)
	}
	return c.GetCollectionByRemoteID(ctx, collection.AccountID, collection.Kind, collection.RemoteID)
}

func (c *Catalog) GetCollectionByRemoteID(ctx context.Context, accountID, kind, remoteID string) (model.Collection, error) {
	row := c.db.QueryRowContext(ctx, collectionSelect+` WHERE c.account_id = ? AND c.kind = ? AND c.remote_id = ? GROUP BY c.id`, accountID, kind, remoteID)
	collection, err := scanCollection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Collection{}, ErrNotFound
	}
	if err != nil {
		return model.Collection{}, fmt.Errorf("get collection: %w", err)
	}
	return collection, nil
}

func (c *Catalog) ListCollections(ctx context.Context, accountID, kind string, includeInactive bool) ([]model.Collection, error) {
	query := collectionSelect + " WHERE (? = '' OR c.account_id = ?) AND (? = '' OR c.kind = ?)"
	args := []any{accountID, accountID, kind, kind}
	if !includeInactive {
		query += " AND c.active = 1"
	}
	query += " GROUP BY c.id ORDER BY c.account_id, c.kind, c.name"
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()
	result := make([]model.Collection, 0)
	for rows.Next() {
		collection, err := scanCollection(rows)
		if err != nil {
			return nil, fmt.Errorf("scan collection: %w", err)
		}
		result = append(result, collection)
	}
	return result, rows.Err()
}

const collectionSelect = `
SELECT c.id, c.account_id, c.kind, c.name, c.remote_id, c.remote_url, c.sync_token,
       c.active, c.created_at, c.last_seen_at, COUNT(o.id)
FROM collections c LEFT JOIN objects o ON o.collection_id = c.id
`

func scanCollection(row rowScanner) (model.Collection, error) {
	var result model.Collection
	var active int
	var created string
	var lastSeen sql.NullString
	if err := row.Scan(&result.ID, &result.AccountID, &result.Kind, &result.Name, &result.RemoteID,
		&result.RemoteURL, &result.SyncToken, &active, &created, &lastSeen, &result.Objects); err != nil {
		return result, err
	}
	var err error
	result.CreatedAt, err = parseTime(created)
	if err != nil {
		return result, err
	}
	result.LastSeenAt, err = parseNullableTime(lastSeen)
	result.Active = active != 0
	return result, err
}

func (c *Catalog) SetCollectionSync(ctx context.Context, id int64, token string) error {
	_, err := c.db.ExecContext(ctx, "UPDATE collections SET sync_token = ? WHERE id = ?", token, id)
	if err != nil {
		return fmt.Errorf("set collection synchronization state: %w", err)
	}
	return nil
}

func (c *Catalog) PutObject(ctx context.Context, object model.Object) (model.Object, error) {
	flags, err := json.Marshal(object.Flags)
	if err != nil {
		return model.Object{}, fmt.Errorf("encode object flags: %w", err)
	}
	remoteCollections, err := json.Marshal(object.RemoteCollections)
	if err != nil {
		return model.Object{}, fmt.Errorf("encode object remote collections: %w", err)
	}
	archivedAt := object.ArchivedAt
	if archivedAt.IsZero() {
		archivedAt = time.Now().UTC()
	}
	_, err = c.db.ExecContext(ctx, `
INSERT INTO objects (collection_id, remote_id, etag, content_type, size, sha256, relative_path, sidecar_path, title, flags_json, internal_date, remote_collections_json, archived_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(collection_id, remote_id) DO UPDATE SET
    etag = excluded.etag, content_type = excluded.content_type, size = excluded.size,
    sha256 = excluded.sha256, relative_path = excluded.relative_path,
    sidecar_path = excluded.sidecar_path, title = excluded.title, flags_json = excluded.flags_json,
    internal_date = excluded.internal_date, remote_collections_json = excluded.remote_collections_json
`, object.CollectionID, object.RemoteID, object.ETag, object.ContentType, object.Size, object.SHA256,
		object.Path, object.SidecarPath, object.Title, string(flags), formatTimePointer(object.InternalDate), string(remoteCollections), formatTime(archivedAt))
	if err != nil {
		return model.Object{}, fmt.Errorf("upsert object %q: %w", object.RemoteID, err)
	}
	return c.GetObjectByRemoteID(ctx, object.CollectionID, object.RemoteID)
}

func (c *Catalog) GetObject(ctx context.Context, id int64) (model.Object, error) {
	object, err := scanObject(c.db.QueryRowContext(ctx, objectSelect+" WHERE o.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return object, ErrNotFound
	}
	if err != nil {
		return object, fmt.Errorf("get object: %w", err)
	}
	return object, nil
}

func (c *Catalog) GetObjectByRemoteID(ctx context.Context, collectionID int64, remoteID string) (model.Object, error) {
	object, err := scanObject(c.db.QueryRowContext(ctx, objectSelect+" WHERE o.collection_id = ? AND o.remote_id = ?", collectionID, remoteID))
	if errors.Is(err, sql.ErrNoRows) {
		return object, ErrNotFound
	}
	if err != nil {
		return object, fmt.Errorf("get object by identity: %w", err)
	}
	return object, nil
}

func (c *Catalog) ListObjects(ctx context.Context, filter model.ObjectFilter) ([]model.Object, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 1000 {
		filter.Limit = 1000
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	query := objectSelect + " WHERE 1 = 1"
	args := make([]any, 0, 6)
	if filter.AccountID != "" {
		query += " AND c.account_id = ?"
		args = append(args, filter.AccountID)
	}
	if filter.Collection != "" {
		query += " AND c.name = ?"
		args = append(args, filter.Collection)
	}
	if filter.Kind != "" {
		query += " AND c.kind = ?"
		args = append(args, filter.Kind)
	}
	query += " ORDER BY o.archived_at DESC, o.id DESC LIMIT ? OFFSET ?"
	args = append(args, filter.Limit, filter.Offset)
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	defer rows.Close()
	result := make([]model.Object, 0)
	for rows.Next() {
		object, err := scanObject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan object: %w", err)
		}
		result = append(result, object)
	}
	return result, rows.Err()
}

func (c *Catalog) AllObjects(ctx context.Context, accountID string) ([]model.Object, error) {
	query := objectSelect
	args := make([]any, 0, 1)
	if accountID != "" {
		query += " WHERE c.account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY o.id"
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list all objects: %w", err)
	}
	defer rows.Close()
	result := make([]model.Object, 0)
	for rows.Next() {
		object, err := scanObject(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, object)
	}
	return result, rows.Err()
}

const objectSelect = `
SELECT o.id, o.collection_id, c.account_id, c.name, c.remote_id, c.kind, o.remote_id, o.etag,
       o.content_type, o.size, o.sha256, o.relative_path, o.sidecar_path, o.title,
       o.flags_json, o.internal_date, o.remote_collections_json,
       o.archived_at, o.last_verified_at, o.verify_error
FROM objects o JOIN collections c ON c.id = o.collection_id
`

func scanObject(row rowScanner) (model.Object, error) {
	var result model.Object
	var archived, flags, remoteCollections string
	var verified, internalDate sql.NullString
	if err := row.Scan(&result.ID, &result.CollectionID, &result.AccountID, &result.Collection, &result.CollectionRemoteID,
		&result.Kind, &result.RemoteID, &result.ETag, &result.ContentType, &result.Size,
		&result.SHA256, &result.Path, &result.SidecarPath, &result.Title, &flags, &internalDate, &remoteCollections, &archived,
		&verified, &result.VerifyError); err != nil {
		return result, err
	}
	var err error
	result.ArchivedAt, err = parseTime(archived)
	if err != nil {
		return result, err
	}
	result.LastVerifiedAt, err = parseNullableTime(verified)
	if err != nil {
		return result, err
	}
	result.InternalDate, err = parseNullableTime(internalDate)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(flags), &result.Flags); err != nil {
		return result, fmt.Errorf("decode object flags: %w", err)
	}
	if err := json.Unmarshal([]byte(remoteCollections), &result.RemoteCollections); err != nil {
		return result, fmt.Errorf("decode object remote collections: %w", err)
	}
	return result, nil
}
