package pimbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/lauritsk/backup/internal/pimbackup/catalog"
	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/pimbackup/dav"
	imapbackup "github.com/lauritsk/backup/internal/pimbackup/imap"
	"github.com/lauritsk/backup/internal/pimbackup/jmap"
	"github.com/lauritsk/backup/internal/pimbackup/mailstore"
	"github.com/lauritsk/backup/internal/pimbackup/model"
	"github.com/lauritsk/backup/internal/pimbackup/objectstore"
)

type remoteCollection struct {
	Name, RemoteID, URL, SyncToken, Kind string
}

type remoteObject struct {
	RemoteID, BlobID, URL, ETag, ContentType string
	Attributes                               objectstore.Attributes
	ComparableETag                           bool
}

type objectSource interface {
	Collections(context.Context) ([]remoteCollection, error)
	Objects(context.Context, remoteCollection) ([]remoteObject, string, error)
	Get(context.Context, remoteObject) (io.ReadCloser, string, error)
	Close()
}

type davObjectSource struct{ client *dav.Client }
type jmapObjectSource struct{ client *jmap.Client }

func newObjectSource(ctx context.Context, account config.AccountConfig) (objectSource, error) {
	switch account.Protocol {
	case "jmap":
		client, err := jmap.New(ctx, account)
		if err != nil {
			return nil, err
		}
		return &jmapObjectSource{client: client}, nil
	case "carddav", "caldav":
		client, err := dav.New(ctx, account)
		if err != nil {
			return nil, err
		}
		return &davObjectSource{client: client}, nil
	default:
		return nil, errors.New("unsupported object protocol " + account.Protocol)
	}
}

func (source *davObjectSource) Collections(ctx context.Context) ([]remoteCollection, error) {
	collections, err := source.client.Collections(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]remoteCollection, 0, len(collections))
	for _, collection := range collections {
		result = append(result, remoteCollection{Name: collection.Name, RemoteID: collection.RemoteID, URL: collection.URL, SyncToken: collection.SyncToken, Kind: collection.Kind})
	}
	return result, nil
}

func (source *davObjectSource) Objects(ctx context.Context, collection remoteCollection) ([]remoteObject, string, error) {
	objects, token, err := source.client.Objects(ctx, dav.Collection{Name: collection.Name, RemoteID: collection.RemoteID, URL: collection.URL, SyncToken: collection.SyncToken, Kind: collection.Kind})
	if err != nil {
		return nil, "", err
	}
	result := make([]remoteObject, 0, len(objects))
	for _, object := range objects {
		result = append(result, remoteObject{RemoteID: object.RemoteID, URL: object.URL, ETag: object.ETag, ContentType: object.ContentType, ComparableETag: object.ETag != ""})
	}
	return result, token, nil
}

func (source *davObjectSource) Get(ctx context.Context, object remoteObject) (io.ReadCloser, string, error) {
	return source.client.Get(ctx, dav.Object{RemoteID: object.RemoteID, URL: object.URL, ETag: object.ETag, ContentType: object.ContentType})
}

func (source *davObjectSource) Close() { source.client.Close() }

func (source *jmapObjectSource) Collections(ctx context.Context) ([]remoteCollection, error) {
	collection, err := source.client.Collection(ctx)
	if err != nil {
		return nil, err
	}
	return []remoteCollection{{Name: collection.Name, RemoteID: collection.RemoteID, URL: collection.URL, Kind: collection.Kind}}, nil
}

func (source *jmapObjectSource) Objects(ctx context.Context, _ remoteCollection) ([]remoteObject, string, error) {
	objects, token, err := source.client.Objects(ctx)
	if err != nil {
		return nil, "", err
	}
	result := make([]remoteObject, 0, len(objects))
	for _, object := range objects {
		result = append(result, remoteObject{
			RemoteID: object.RemoteID, BlobID: object.BlobID, ETag: object.ETag, ContentType: object.ContentType,
			ComparableETag: true,
			Attributes:     objectstore.Attributes{Flags: object.Flags, InternalDate: object.ReceivedAt, RemoteCollections: object.MailboxIDs},
		})
	}
	return result, token, nil
}

func (source *jmapObjectSource) Get(ctx context.Context, object remoteObject) (io.ReadCloser, string, error) {
	body, err := source.client.Get(ctx, jmap.Object{RemoteID: object.RemoteID, BlobID: object.BlobID, ETag: object.ETag, ContentType: object.ContentType})
	return body, object.ContentType, err
}

func (source *jmapObjectSource) Close() { source.client.Close() }

func (s *Service) backup(ctx context.Context, request model.BackupRequest) (model.BackupReport, error) {
	accounts, err := s.selectedAccounts(request.Accounts)
	if err != nil {
		return model.BackupReport{}, err
	}
	if len(accounts) == 0 {
		return model.BackupReport{}, errors.New("no enabled PIM accounts are configured")
	}

	report := model.BackupReport{}
	for _, account := range accounts {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		accountResult := s.backupAccount(ctx, account)
		report.Accounts = append(report.Accounts, accountResult)
		report.Fetched += accountResult.Fetched
		report.Bytes += accountResult.Bytes
		if accountResult.Error != "" {
			report.Errors++
		}
		for _, mailbox := range accountResult.Mailboxes {
			if mailbox.Error != "" {
				report.Errors++
			}
		}
		for _, collection := range accountResult.Collections {
			if collection.Error != "" {
				report.Errors++
			}
		}
	}
	if report.Errors > 0 {
		return report, fmt.Errorf("backup completed with %d error(s)", report.Errors)
	}
	return report, nil
}

func (s *Service) backupAccount(ctx context.Context, account config.AccountConfig) model.AccountBackupResult {
	if account.Protocol == "imap" {
		return s.backupIMAPAccount(ctx, account)
	}
	if account.Protocol == "jmap" || account.Protocol == "carddav" || account.Protocol == "caldav" {
		return s.backupObjectAccount(ctx, account)
	}
	return model.AccountBackupResult{AccountID: account.ID, Error: "unsupported protocol " + account.Protocol}
}

func (s *Service) backupIMAPAccount(ctx context.Context, account config.AccountConfig) (result model.AccountBackupResult) {
	result.AccountID = account.ID
	if err := s.catalog.UpsertAccount(ctx, account.ID, account.Protocol); err != nil {
		result.Error = err.Error()
		return
	}
	if account.ResolvedPassword == "" {
		result.Error = "password is not configured"
		return
	}
	remote, err := s.dialer.Dial(ctx, account)
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer func() {
		if closeErr := remote.Close(); closeErr != nil && result.Error == "" {
			result.Error = "close IMAP connection: " + closeErr.Error()
		}
	}()

	mailboxes, err := remote.ListMailboxes(ctx)
	if err != nil {
		result.Error = err.Error()
		return
	}
	selectedMailboxes := 0
	for _, remoteMailbox := range mailboxes {
		if err := ctx.Err(); err != nil {
			result.Error = err.Error()
			return
		}
		if !remoteMailbox.Selectable || !selected(remoteMailbox.Name, account.Mailboxes, account.ExcludeMailboxes) {
			continue
		}
		selectedMailboxes++
		mailboxResult := s.backupMailbox(ctx, account, remote, remoteMailbox)
		result.Mailboxes = append(result.Mailboxes, mailboxResult)
		result.Fetched += mailboxResult.Fetched
		result.Bytes += mailboxResult.Bytes
	}
	if selectedMailboxes == 0 {
		result.Error = "no selectable IMAP mailboxes matched the configured patterns"
	}
	return
}

func (s *Service) backupObjectAccount(ctx context.Context, account config.AccountConfig) (result model.AccountBackupResult) {
	result.AccountID = account.ID
	if err := s.catalog.UpsertAccount(ctx, account.ID, account.Protocol); err != nil {
		result.Error = err.Error()
		return
	}
	source, err := newObjectSource(ctx, account)
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer source.Close()
	collections, err := source.Collections(ctx)
	if err != nil {
		result.Error = err.Error()
		return
	}
	for _, remote := range collections {
		if !selected(remote.Name, account.Collections, account.ExcludeCollections) {
			continue
		}
		entry := s.backupObjectCollection(ctx, account, source, remote)
		result.Collections = append(result.Collections, entry)
		result.Fetched += entry.Fetched
		result.Bytes += entry.Bytes
	}
	if len(result.Collections) == 0 {
		family := "DAV"
		if account.Protocol == "jmap" {
			family = "JMAP"
		}
		result.Error = "no " + family + " collections matched the configured patterns"
	}
	return
}

func (s *Service) backupObjectCollection(ctx context.Context, account config.AccountConfig, source objectSource, remote remoteCollection) (entry model.CollectionBackupResult) {
	entry.Collection, entry.Kind = remote.Name, remote.Kind
	collection, err := s.catalog.EnsureCollection(ctx, model.Collection{AccountID: account.ID, Kind: remote.Kind, Name: remote.Name, RemoteID: remote.RemoteID, RemoteURL: remote.URL, SyncToken: remote.SyncToken})
	if err != nil {
		entry.Error = err.Error()
		return
	}
	objects, token, err := source.Objects(ctx, remote)
	if err != nil {
		entry.Error = err.Error()
		return
	}
	entry.Found = len(objects)
	for _, remoteObject := range objects {
		if err := ctx.Err(); err != nil {
			entry.Error = err.Error()
			break
		}
		existing, lookupErr := s.catalog.GetObjectByRemoteID(ctx, collection.ID, remoteObject.RemoteID)
		if lookupErr == nil && remoteObject.ComparableETag && existing.ETag == remoteObject.ETag {
			if verifyErr := s.objectStore.BasicCheck(existing); verifyErr == nil {
				continue
			}
		} else if lookupErr != nil && !errors.Is(lookupErr, catalog.ErrNotFound) {
			entry.Error = lookupErr.Error()
			break
		}
		body, contentType, getErr := source.Get(ctx, remoteObject)
		if getErr != nil {
			entry.Error = getErr.Error()
			break
		}
		saved, saveErr := s.objectStore.Save(ctx, collection, remoteObject.RemoteID, remoteObject.ETag, contentType, remoteObject.Attributes, body)
		saveErr = errors.Join(saveErr, body.Close())
		if saveErr != nil {
			entry.Error = saveErr.Error()
			break
		}
		stored, putErr := s.catalog.PutObject(ctx, saved.Object)
		if putErr != nil {
			entry.Error = putErr.Error()
			break
		}
		entry.Fetched++
		entry.Bytes += stored.Size
	}
	if entry.Error == "" {
		if err := s.setCollectionSync(ctx, collection, token); err != nil {
			entry.Error = err.Error()
		}
	}
	return
}

func (s *Service) setCollectionSync(ctx context.Context, collection model.Collection, token string) error {
	if err := s.catalog.SetCollectionSync(ctx, collection.ID, token); err != nil {
		return err
	}
	collection.SyncToken = token
	_, err := s.objectStore.PrepareCollection(collection)
	return err
}

func selected(name string, include, exclude []string) bool {
	if !matchesAny(name, include) {
		return false
	}
	return !matchesAny(name, exclude)
}

func matchesAny(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == "*" {
			return true
		}
		if matched, _ := path.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

func protocolForKind(kind string) string {
	switch kind {
	case "contact":
		return "carddav"
	case "calendar":
		return "caldav"
	case "mail":
		return "jmap"
	default:
		return "unknown"
	}
}

func (s *Service) backupMailbox(ctx context.Context, account config.AccountConfig, remote imapbackup.Remote, remoteMailbox imapbackup.Mailbox) model.MailboxBackupResult {
	result := model.MailboxBackupResult{Mailbox: remoteMailbox.Name}
	selectedMailbox, err := remote.SelectMailbox(ctx, remoteMailbox.Name)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.UIDValidity = selectedMailbox.UIDValidity

	mailbox, err := s.catalog.EnsureMailbox(ctx, model.Mailbox{
		AccountID: account.ID, Name: remoteMailbox.Name, PathKey: mailstore.PathKey(remoteMailbox.Name), Delimiter: remoteMailbox.Delimiter,
		UIDValidity: selectedMailbox.UIDValidity, RemoteMessages: selectedMailbox.Messages,
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if _, err := s.store.PrepareMailbox(mailbox); err != nil {
		result.Error = err.Error()
		return result
	}

	uids, err := remote.SearchUIDsAfter(ctx, mailbox.LastUID)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Found = len(uids)
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			result.Error = err.Error()
			return result
		}
		var saved mailstore.SavedMessage
		err := remote.FetchMessage(ctx, uid, func(fetched imapbackup.FetchedMessage, body io.Reader) error {
			var saveErr error
			saved, saveErr = s.store.Save(ctx, mailbox, mailstore.FetchedMessage{UID: fetched.UID, InternalDate: fetched.InternalDate, Flags: fetched.Flags, ExpectedSize: fetched.Size, Body: body})
			return saveErr
		})
		if err != nil {
			result.Error = err.Error()
			return result
		}
		stored, err := s.catalog.PutMessage(ctx, saved.Message)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Fetched++
		result.Bytes += stored.Size
		s.logger.Debug("archived IMAP message", "account", account.ID, "mailbox", remoteMailbox.Name, "uid_validity", selectedMailbox.UIDValidity, "uid", uid, "bytes", stored.Size, "created", saved.Created)
	}
	return result
}

func (s *Service) selectedAccounts(ids []string) ([]config.AccountConfig, error) {
	if len(ids) == 0 {
		return s.config.EnabledAccounts(), nil
	}
	seen := make(map[string]struct{})
	accounts := make([]config.AccountConfig, 0, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		account, found := s.config.Account(id)
		if !found {
			return nil, fmt.Errorf("account %q is not configured", id)
		}
		if account.Disabled {
			return nil, fmt.Errorf("account %q is disabled", id)
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}
