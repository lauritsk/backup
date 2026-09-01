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

func (c *Catalog) UpsertAccount(ctx context.Context, id, protocol string) error {
	now := formatTime(time.Now().UTC())
	_, err := c.db.ExecContext(ctx, `
INSERT INTO accounts (id, protocol, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET protocol = excluded.protocol, updated_at = excluded.updated_at
`, id, protocol, now, now)
	if err != nil {
		return fmt.Errorf("upsert account %q: %w", id, err)
	}
	return nil
}

func (c *Catalog) ListAccounts(ctx context.Context) ([]model.Account, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT a.id, a.protocol, a.created_at, a.updated_at,
       (SELECT COUNT(*) FROM mailboxes mb WHERE mb.account_id = a.id),
       (SELECT COUNT(*) FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id WHERE mb.account_id = a.id),
       (SELECT COUNT(*) FROM collections c WHERE c.account_id = a.id),
       (SELECT COUNT(*) FROM objects o JOIN collections c ON c.id = o.collection_id WHERE c.account_id = a.id)
FROM accounts a
ORDER BY a.id
`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	accounts := make([]model.Account, 0)
	for rows.Next() {
		var account model.Account
		var createdAt, updatedAt string
		if err := rows.Scan(&account.ID, &account.Protocol, &createdAt, &updatedAt, &account.Mailboxes, &account.Messages, &account.Collections, &account.Objects); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		account.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		account.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return accounts, nil
}

func (c *Catalog) EnsureMailbox(ctx context.Context, mailbox model.Mailbox) (model.Mailbox, error) {
	now := time.Now().UTC()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Mailbox{}, fmt.Errorf("begin mailbox transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
UPDATE mailboxes SET active = 0
WHERE account_id = ? AND name = ? AND uid_validity <> ? AND active = 1
`, mailbox.AccountID, mailbox.Name, mailbox.UIDValidity); err != nil {
		return model.Mailbox{}, fmt.Errorf("retire old mailbox generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO mailboxes (
    account_id, name, path_key, delimiter, uid_validity, last_uid,
    remote_messages, active, created_at, last_seen_at
) VALUES (?, ?, ?, ?, ?, 0, ?, 1, ?, ?)
ON CONFLICT(account_id, name, uid_validity) DO UPDATE SET
    path_key = excluded.path_key,
    delimiter = excluded.delimiter,
    remote_messages = excluded.remote_messages,
    active = 1,
    last_seen_at = excluded.last_seen_at
`, mailbox.AccountID, mailbox.Name, mailbox.PathKey, mailbox.Delimiter, mailbox.UIDValidity,
		mailbox.RemoteMessages, formatTime(now), formatTime(now)); err != nil {
		return model.Mailbox{}, fmt.Errorf("upsert mailbox %q: %w", mailbox.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return model.Mailbox{}, fmt.Errorf("commit mailbox transaction: %w", err)
	}
	return c.GetMailbox(ctx, mailbox.AccountID, mailbox.Name, mailbox.UIDValidity)
}

func (c *Catalog) GetMailbox(ctx context.Context, accountID, name string, uidValidity uint32) (model.Mailbox, error) {
	row := c.db.QueryRowContext(ctx, mailboxSelect+`
WHERE mb.account_id = ? AND mb.name = ? AND mb.uid_validity = ?
GROUP BY mb.id
`, accountID, name, uidValidity)
	mailbox, err := scanMailbox(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Mailbox{}, ErrNotFound
	}
	if err != nil {
		return model.Mailbox{}, fmt.Errorf("get mailbox: %w", err)
	}
	return mailbox, nil
}

func (c *Catalog) ListMailboxes(ctx context.Context, accountID string, includeInactive bool) ([]model.Mailbox, error) {
	query := mailboxSelect + "\nWHERE (? = '' OR mb.account_id = ?)"
	args := []any{accountID, accountID}
	if !includeInactive {
		query += " AND mb.active = 1"
	}
	query += "\nGROUP BY mb.id ORDER BY mb.account_id, mb.name, mb.uid_validity DESC"
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	defer rows.Close()

	mailboxes := make([]model.Mailbox, 0)
	for rows.Next() {
		mailbox, err := scanMailbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan mailbox: %w", err)
		}
		mailboxes = append(mailboxes, mailbox)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	return mailboxes, nil
}

const mailboxSelect = `
SELECT mb.id, mb.account_id, mb.name, mb.path_key, mb.delimiter,
       mb.uid_validity, mb.last_uid, mb.remote_messages, mb.active,
       mb.created_at, mb.last_seen_at, COUNT(m.id)
FROM mailboxes mb
LEFT JOIN messages m ON m.mailbox_id = mb.id
`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMailbox(row rowScanner) (model.Mailbox, error) {
	var mailbox model.Mailbox
	var active int
	var createdAt string
	var lastSeen sql.NullString
	if err := row.Scan(
		&mailbox.ID, &mailbox.AccountID, &mailbox.Name, &mailbox.PathKey, &mailbox.Delimiter,
		&mailbox.UIDValidity, &mailbox.LastUID, &mailbox.RemoteMessages, &active,
		&createdAt, &lastSeen, &mailbox.Messages,
	); err != nil {
		return model.Mailbox{}, err
	}
	var err error
	mailbox.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.Mailbox{}, err
	}
	mailbox.LastSeenAt, err = parseNullableTime(lastSeen)
	if err != nil {
		return model.Mailbox{}, err
	}
	mailbox.Active = active != 0
	return mailbox, nil
}

func (c *Catalog) PutMessage(ctx context.Context, message model.Message) (model.Message, error) {
	flags, err := json.Marshal(message.Flags)
	if err != nil {
		return model.Message{}, fmt.Errorf("encode message flags: %w", err)
	}
	archivedAt := message.ArchivedAt
	if archivedAt.IsZero() {
		archivedAt = time.Now().UTC()
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, fmt.Errorf("begin message transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
INSERT INTO messages (
    mailbox_id, uid, internal_date, size, sha256, relative_path, sidecar_path,
    flags_json, subject, from_value, to_value, header_message_id, header_date,
    parse_error, archived_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(mailbox_id, uid) DO UPDATE SET
    internal_date = excluded.internal_date,
    size = excluded.size,
    sha256 = excluded.sha256,
    relative_path = excluded.relative_path,
    sidecar_path = excluded.sidecar_path,
    flags_json = excluded.flags_json,
    subject = excluded.subject,
    from_value = excluded.from_value,
    to_value = excluded.to_value,
    header_message_id = excluded.header_message_id,
    header_date = excluded.header_date,
    parse_error = excluded.parse_error
`, message.MailboxID, message.UID, formatTimePointer(message.InternalDate), message.Size,
		message.SHA256, message.Path, message.SidecarPath, string(flags), message.Subject,
		message.From, message.To, message.HeaderMessageID, formatTimePointer(message.HeaderDate),
		message.ParseError, formatTime(archivedAt))
	if err != nil {
		return model.Message{}, fmt.Errorf("upsert message UID %d: %w", message.UID, err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE mailboxes
SET last_uid = CASE WHEN last_uid < ? THEN ? ELSE last_uid END
WHERE id = ?
`, message.UID, message.UID, message.MailboxID); err != nil {
		return model.Message{}, fmt.Errorf("advance mailbox state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Message{}, fmt.Errorf("commit message transaction: %w", err)
	}
	return c.GetMessageByIdentity(ctx, message.MailboxID, message.UID)
}

func (c *Catalog) GetMessage(ctx context.Context, id int64) (model.Message, error) {
	row := c.db.QueryRowContext(ctx, messageSelect+" WHERE m.id = ?", id)
	message, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, ErrNotFound
	}
	if err != nil {
		return model.Message{}, fmt.Errorf("get message: %w", err)
	}
	return message, nil
}

func (c *Catalog) GetMessageByIdentity(ctx context.Context, mailboxID int64, uid uint32) (model.Message, error) {
	row := c.db.QueryRowContext(ctx, messageSelect+" WHERE m.mailbox_id = ? AND m.uid = ?", mailboxID, uid)
	message, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, ErrNotFound
	}
	if err != nil {
		return model.Message{}, fmt.Errorf("get message by identity: %w", err)
	}
	return message, nil
}

func (c *Catalog) ListMessages(ctx context.Context, filter model.MessageFilter) ([]model.Message, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 1000 {
		filter.Limit = 1000
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	query := messageSelect + " WHERE 1 = 1"
	var args []any
	if filter.AccountID != "" {
		query += " AND mb.account_id = ?"
		args = append(args, filter.AccountID)
	}
	if filter.Mailbox != "" {
		query += " AND mb.name = ?"
		args = append(args, filter.Mailbox)
	}
	if filter.UIDValidity != 0 {
		query += " AND mb.uid_validity = ?"
		args = append(args, filter.UIDValidity)
	}
	query += " ORDER BY m.internal_date DESC, m.id DESC LIMIT ? OFFSET ?"
	args = append(args, filter.Limit, filter.Offset)

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	messages := make([]model.Message, 0)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	return messages, nil
}

func (c *Catalog) AllMessages(ctx context.Context, accountID string) ([]model.Message, error) {
	query := messageSelect
	var args []any
	if accountID != "" {
		query += " WHERE mb.account_id = ?"
		args = append(args, accountID)
	}
	query += " ORDER BY m.id"
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list all messages: %w", err)
	}
	defer rows.Close()
	messages := make([]model.Message, 0)
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all messages: %w", err)
	}
	return messages, nil
}

const messageSelect = `
SELECT m.id, m.mailbox_id, mb.account_id, mb.name, mb.uid_validity, m.uid,
       m.internal_date, m.size, m.sha256, m.relative_path, m.sidecar_path,
       m.flags_json, m.subject, m.from_value, m.to_value, m.header_message_id,
       m.header_date, m.parse_error, m.archived_at, m.last_verified_at,
       m.verify_error
FROM messages m
JOIN mailboxes mb ON mb.id = m.mailbox_id
`

func scanMessage(row rowScanner) (model.Message, error) {
	var message model.Message
	var internalDate, headerDate, lastVerified sql.NullString
	var archivedAt, flags string
	if err := row.Scan(
		&message.ID, &message.MailboxID, &message.AccountID, &message.Mailbox,
		&message.UIDValidity, &message.UID, &internalDate, &message.Size, &message.SHA256,
		&message.Path, &message.SidecarPath, &flags, &message.Subject, &message.From,
		&message.To, &message.HeaderMessageID, &headerDate, &message.ParseError,
		&archivedAt, &lastVerified, &message.VerifyError,
	); err != nil {
		return model.Message{}, err
	}
	var err error
	message.InternalDate, err = parseNullableTime(internalDate)
	if err != nil {
		return model.Message{}, err
	}
	message.HeaderDate, err = parseNullableTime(headerDate)
	if err != nil {
		return model.Message{}, err
	}
	message.LastVerifiedAt, err = parseNullableTime(lastVerified)
	if err != nil {
		return model.Message{}, err
	}
	message.ArchivedAt, err = parseTime(archivedAt)
	if err != nil {
		return model.Message{}, err
	}
	if err := json.Unmarshal([]byte(flags), &message.Flags); err != nil {
		return model.Message{}, fmt.Errorf("decode message flags: %w", err)
	}
	return message, nil
}
