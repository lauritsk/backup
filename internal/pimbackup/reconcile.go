package pimbackup

import (
	"context"
	"errors"

	"github.com/lauritsk/backup/internal/pimbackup/catalog"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

func (s *Service) reconcile(ctx context.Context) error {
	removed, err := s.store.CleanupTemps(ctx)
	if err != nil {
		return err
	}
	if len(removed) > 0 {
		s.logger.Info("removed incomplete temporary mail files", "files", len(removed))
	}
	if err := s.recoverSidecars(ctx); err != nil {
		return err
	}

	scan := s.store.Scan(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, issue := range scan.Errors {
		s.logger.Warn("canonical mail scan found an issue", "error", issue)
	}
	for _, scanned := range scan.Mailboxes {
		metadata := scanned.Metadata
		if err := s.catalog.UpsertAccount(ctx, metadata.AccountID, "imap"); err != nil {
			return err
		}
		existingMailbox, lookupErr := s.catalog.GetMailbox(ctx, metadata.AccountID, metadata.Mailbox, metadata.UIDValidity)
		originalLastUID := existingMailbox.LastUID
		if lookupErr != nil && !errors.Is(lookupErr, catalog.ErrNotFound) {
			return lookupErr
		}
		mailbox, err := s.catalog.EnsureMailbox(ctx, model.Mailbox{
			AccountID: metadata.AccountID, Name: metadata.Mailbox, PathKey: metadata.PathKey, Delimiter: metadata.Delimiter,
			UIDValidity: metadata.UIDValidity, RemoteMessages: existingMailbox.RemoteMessages,
		})
		if err != nil {
			return err
		}
		for _, message := range scanned.Messages {
			message.MailboxID = mailbox.ID
			if _, err := s.catalog.PutMessage(ctx, message); err != nil {
				return err
			}
		}
		if err := s.catalog.SetMailboxSync(ctx, mailbox.ID, originalLastUID); err != nil {
			return err
		}
	}
	if len(scan.Errors) > 0 {
		s.logger.Warn("resetting IMAP cursors because canonical file errors require conservative reconciliation")
		if err := s.catalog.ResetSyncStates(ctx); err != nil {
			return err
		}
	}

	objectScan := s.objectStore.Scan(ctx)
	for _, issue := range objectScan.Errors {
		s.logger.Warn("canonical PIM object scan found an issue", "error", issue)
	}
	for _, scanned := range objectScan.Collections {
		metadata := scanned.Metadata
		if err := s.catalog.UpsertAccount(ctx, metadata.AccountID, protocolForKind(metadata.Kind)); err != nil {
			return err
		}
		collection, err := s.catalog.EnsureCollection(ctx, model.Collection{AccountID: metadata.AccountID, Kind: metadata.Kind, Name: metadata.Name, RemoteID: metadata.RemoteID, RemoteURL: metadata.RemoteURL, SyncToken: metadata.SyncToken})
		if err != nil {
			return err
		}
		for _, object := range scanned.Objects {
			object.CollectionID = collection.ID
			if _, err := s.catalog.PutObject(ctx, object); err != nil {
				return err
			}
		}
	}
	if len(objectScan.Errors) > 0 {
		s.logger.Warn("resetting PIM object cursors because canonical file errors require conservative reconciliation")
		if err := s.catalog.ResetObjectSyncStates(ctx); err != nil {
			return err
		}
	}

	messages, err := s.catalog.AllMessages(ctx, "")
	if err != nil {
		return err
	}
	for _, message := range messages {
		if err := s.store.BasicCheck(message); err != nil {
			s.logger.Warn("rewinding mailbox with missing or invalid canonical data", "message_id", message.ID, "account", message.AccountID, "mailbox", message.Mailbox, "uid", message.UID, "error", err)
			if rewindErr := s.catalog.RewindMailbox(ctx, message.MailboxID, message.UID); rewindErr != nil {
				return rewindErr
			}
		}
	}
	objects, err := s.catalog.AllObjects(ctx, "")
	if err != nil {
		return err
	}
	for _, object := range objects {
		if err := s.objectStore.BasicCheck(object); err != nil {
			s.logger.Warn("resetting collection with missing or invalid canonical data", "object_id", object.ID, "account", object.AccountID, "collection", object.Collection, "error", err)
			if resetErr := s.catalog.SetCollectionSync(ctx, object.CollectionID, ""); resetErr != nil {
				return resetErr
			}
		}
	}
	return nil
}

func (s *Service) recoverSidecars(ctx context.Context) error {
	known, err := s.catalog.AllMessages(ctx, "")
	if err != nil {
		return err
	}
	recovered, problems := s.store.RecoverMissingSidecars(ctx, known)
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(recovered) > 0 {
		s.logger.Warn("recovered message metadata missing after an incomplete commit", "sidecars", len(recovered))
	}
	for _, problem := range problems {
		s.logger.Warn("could not recover missing message metadata", "error", problem)
	}
	return nil
}
