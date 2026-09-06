package pimbackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lauritsk/backup/internal/atomicfile"
	"github.com/lauritsk/backup/internal/pimbackup/dav"
	"github.com/lauritsk/backup/internal/pimbackup/jmap"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

func (s *Service) Check(ctx context.Context) model.CheckReport {
	report := model.CheckReport{Status: "ok"}
	appendCheck := func(name string, started time.Time, err error) {
		result := model.CheckResult{Name: name, Status: "ok", Duration: time.Since(started)}
		if err != nil {
			result.Status = "error"
			result.Message = s.cleanError(err)
			report.Status = "error"
		}
		report.Checks = append(report.Checks, result)
	}

	started := time.Now()
	appendCheck("storage", started, s.checkStorage())
	started = time.Now()
	appendCheck("sqlite", started, s.catalog.QuickCheck(ctx))
	for _, account := range s.config.EnabledAccounts() {
		started = time.Now()
		var checkErr error
		switch account.Protocol {
		case "imap":
			remote, err := s.dialer.Dial(ctx, account)
			if err != nil {
				checkErr = err
			} else {
				_, checkErr = remote.ListMailboxes(ctx)
				checkErr = errors.Join(checkErr, remote.Close())
			}
		case "jmap":
			client, err := jmap.New(ctx, account)
			if err != nil {
				checkErr = err
			} else {
				_, checkErr = client.Collection(ctx)
				client.Close()
			}
		case "carddav", "caldav":
			client, err := dav.New(ctx, account)
			if err != nil {
				checkErr = err
			} else {
				_, checkErr = client.Collections(ctx)
				client.Close()
			}
		default:
			checkErr = fmt.Errorf("unsupported protocol %q", account.Protocol)
		}
		appendCheck("account:"+account.ID, started, checkErr)
	}
	return report
}

func (s *Service) checkStorage() error {
	temp, err := os.CreateTemp(s.config.DataDir, ".pimbackup-check-")
	if err != nil {
		return fmt.Errorf("create storage test file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.WriteString("ok"); err != nil {
		temp.Close()
		return fmt.Errorf("write storage test file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync storage test file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close storage test file: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove storage test file: %w", err)
	}
	return atomicfile.SyncDir(s.config.DataDir)
}

func (s *Service) Rebuild(ctx context.Context) (report model.RebuildReport, resultErr error) {
	release, err := s.gate.TryAcquire()
	if err != nil {
		return model.RebuildReport{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	if err := s.ensureInitialized(ctx); err != nil {
		return model.RebuildReport{}, err
	}
	if err := s.recoverSidecars(ctx); err != nil {
		return model.RebuildReport{}, err
	}
	scan := s.store.Scan(ctx)
	if err := ctx.Err(); err != nil {
		return model.RebuildReport{}, err
	}
	report = model.RebuildReport{}
	for _, scanErr := range scan.Errors {
		report.Errors = append(report.Errors, s.cleanError(scanErr))
	}
	if err := s.catalog.PrepareRebuild(ctx); err != nil {
		return report, err
	}
	for _, scanned := range scan.Mailboxes {
		metadata := scanned.Metadata
		if err := s.catalog.UpsertAccount(ctx, metadata.AccountID, "imap"); err != nil {
			return report, err
		}
		mailbox, err := s.catalog.EnsureMailbox(ctx, model.Mailbox{AccountID: metadata.AccountID, Name: metadata.Mailbox, PathKey: metadata.PathKey, Delimiter: metadata.Delimiter, UIDValidity: metadata.UIDValidity})
		if err != nil {
			return report, err
		}
		report.Mailboxes++
		for _, message := range scanned.Messages {
			message.MailboxID = mailbox.ID
			if _, err := s.catalog.PutMessage(ctx, message); err != nil {
				return report, err
			}
			report.Messages++
		}
	}
	objectScan := s.objectStore.Scan(ctx)
	for _, scanErr := range objectScan.Errors {
		report.Errors = append(report.Errors, s.cleanError(scanErr))
	}
	for _, scanned := range objectScan.Collections {
		metadata := scanned.Metadata
		if err := s.catalog.UpsertAccount(ctx, metadata.AccountID, protocolForKind(metadata.Kind)); err != nil {
			return report, err
		}
		collection, err := s.catalog.EnsureCollection(ctx, model.Collection{AccountID: metadata.AccountID, Kind: metadata.Kind, Name: metadata.Name, RemoteID: metadata.RemoteID, RemoteURL: metadata.RemoteURL, SyncToken: metadata.SyncToken})
		if err != nil {
			return report, err
		}
		report.Collections++
		for _, object := range scanned.Objects {
			object.CollectionID = collection.ID
			if _, err := s.catalog.PutObject(ctx, object); err != nil {
				return report, err
			}
			report.Objects++
		}
	}
	if err := s.catalog.ResetSyncStates(ctx); err != nil {
		return report, err
	}
	for _, account := range s.config.Accounts {
		if err := s.catalog.UpsertAccount(ctx, account.ID, account.Protocol); err != nil {
			return report, err
		}
	}
	if len(report.Errors) > 0 {
		return report, fmt.Errorf("catalog rebuilt with %d canonical file error(s)", len(report.Errors))
	}
	return report, nil
}
