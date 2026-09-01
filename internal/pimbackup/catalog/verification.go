package catalog

import (
	"context"
	"fmt"
	"time"
)

func (c *Catalog) UpdateObjectVerification(ctx context.Context, objectID int64, verifyErr error) error {
	text := ""
	if verifyErr != nil {
		text = verifyErr.Error()
	}
	result, err := c.db.ExecContext(ctx, "UPDATE objects SET last_verified_at = ?, verify_error = ? WHERE id = ?", formatTime(time.Now().UTC()), text, objectID)
	if err != nil {
		return fmt.Errorf("update object verification: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (c *Catalog) UpdateVerification(ctx context.Context, messageID int64, verifyErr error) error {
	errorText := ""
	if verifyErr != nil {
		errorText = verifyErr.Error()
	}
	result, err := c.db.ExecContext(ctx, `
UPDATE messages SET last_verified_at = ?, verify_error = ? WHERE id = ?
`, formatTime(time.Now().UTC()), errorText, messageID)
	if err != nil {
		return fmt.Errorf("update message verification: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read verification update count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}
