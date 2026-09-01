package catalog

import (
	"context"
	"fmt"
)

func (c *Catalog) RewindMailbox(ctx context.Context, mailboxID int64, uid uint32) error {
	before := uint32(0)
	if uid > 1 {
		before = uid - 1
	}
	_, err := c.db.ExecContext(ctx, `
UPDATE mailboxes SET last_uid = CASE WHEN last_uid > ? THEN ? ELSE last_uid END WHERE id = ?
`, before, before, mailboxID)
	if err != nil {
		return fmt.Errorf("rewind mailbox state: %w", err)
	}
	return nil
}

func (c *Catalog) PrepareRebuild(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog rebuild: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM objects"); err != nil {
		return fmt.Errorf("clear objects: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM collections"); err != nil {
		return fmt.Errorf("clear collections: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM messages"); err != nil {
		return fmt.Errorf("clear messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM mailboxes"); err != nil {
		return fmt.Errorf("clear mailboxes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM accounts"); err != nil {
		return fmt.Errorf("clear accounts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit catalog clear: %w", err)
	}
	return nil
}

func (c *Catalog) SetMailboxSync(ctx context.Context, mailboxID int64, lastUID uint32) error {
	if _, err := c.db.ExecContext(ctx, "UPDATE mailboxes SET last_uid = ? WHERE id = ?", lastUID, mailboxID); err != nil {
		return fmt.Errorf("set mailbox synchronization state: %w", err)
	}
	return nil
}

func (c *Catalog) ResetMailboxSync(ctx context.Context, mailboxID int64) error {
	return c.SetMailboxSync(ctx, mailboxID, 0)
}

func (c *Catalog) ResetObjectSyncStates(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, "UPDATE collections SET sync_token = ''"); err != nil {
		return fmt.Errorf("reset collection synchronization state: %w", err)
	}
	return nil
}

func (c *Catalog) ResetSyncStates(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, "UPDATE mailboxes SET last_uid = 0"); err != nil {
		return fmt.Errorf("reset mailbox synchronization state: %w", err)
	}
	if _, err := c.db.ExecContext(ctx, "UPDATE collections SET sync_token = ''"); err != nil {
		return fmt.Errorf("reset collection synchronization state: %w", err)
	}
	return nil
}
