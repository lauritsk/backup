package pimbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"sync"

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

const (
	imapBatchSize   = 10
	objectTransfers = 4
)

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

func (source *jmapObjectSource) Objects(ctx context.Context, collection remoteCollection) ([]remoteObject, string, error) {
	objects, token, err := source.client.Objects(ctx, collection.SyncToken)
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
		result.Error = s.cleanError(err)
		return
	}
	if account.ResolvedPassword == "" {
		result.Error = "password is not configured"
		return
	}
	remote, err := s.dialer.Dial(ctx, account)
	if err != nil {
		result.Error = s.cleanError(err)
		return
	}
	defer func() {
		if closeErr := remote.Close(); closeErr != nil && result.Error == "" {
			result.Error = s.cleanError(fmt.Errorf("close IMAP connection: %w", closeErr))
		}
	}()

	mailboxes, err := remote.ListMailboxes(ctx)
	if err != nil {
		result.Error = s.cleanError(err)
		return
	}
	selectedMailboxes := 0
	for _, remoteMailbox := range mailboxes {
		if err := ctx.Err(); err != nil {
			result.Error = s.cleanError(err)
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
		result.Error = s.cleanError(err)
		return
	}
	source, err := newObjectSource(ctx, account)
	if err != nil {
		result.Error = s.cleanError(err)
		return
	}
	defer source.Close()
	collections, err := source.Collections(ctx)
	if err != nil {
		result.Error = s.cleanError(err)
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
		entry.Error = s.cleanError(err)
		return
	}
	remote.SyncToken = collection.SyncToken
	objects, token, err := source.Objects(ctx, remote)
	if err != nil {
		entry.Error = s.cleanError(err)
		return
	}
	entry.Found = len(objects)
	pending := make([]remoteObject, 0, len(objects))
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			entry.Error = s.cleanError(err)
			break
		}
		existing, lookupErr := s.catalog.GetObjectByRemoteID(ctx, collection.ID, object.RemoteID)
		if lookupErr == nil && object.ComparableETag && existing.ETag == object.ETag {
			if verifyErr := s.objectStore.BasicCheck(existing); verifyErr == nil {
				continue
			}
		} else if lookupErr != nil && !errors.Is(lookupErr, catalog.ErrNotFound) {
			entry.Error = s.cleanError(lookupErr)
			break
		}
		pending = append(pending, object)
	}
	if entry.Error == "" {
		fetched, bytes, fetchErr := s.fetchObjects(ctx, source, collection, pending)
		entry.Fetched, entry.Bytes = fetched, bytes
		if fetchErr != nil {
			entry.Error = s.cleanError(fetchErr)
		}
	}
	if entry.Error == "" {
		if err := s.setCollectionSync(ctx, collection, token); err != nil {
			entry.Error = s.cleanError(err)
		}
	}
	return
}

type objectFetchTask struct {
	index  int
	object remoteObject
}

type objectFetchResult struct {
	index    int
	object   model.Object
	received bool
	err      error
}

func (s *Service) fetchObjects(ctx context.Context, source objectSource, collection model.Collection, objects []remoteObject) (int, int64, error) {
	if len(objects) == 0 {
		return 0, 0, nil
	}
	workers := objectTransfers
	if len(objects) < workers {
		workers = len(objects)
	}
	jobs := make(chan objectFetchTask)
	results := make(chan objectFetchResult, len(objects))
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for task := range jobs {
				result := s.fetchObject(ctx, source, collection, task.object)
				result.index = task.index
				result.received = true
				results <- result
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index, object := range objects {
			select {
			case jobs <- objectFetchTask{index: index, object: object}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		group.Wait()
		close(results)
	}()

	ordered := make([]objectFetchResult, len(objects))
	for result := range results {
		ordered[result.index] = result
	}
	var fetched int
	var bytes int64
	var resultErr error
	for _, result := range ordered {
		if !result.received {
			continue
		}
		if result.err != nil {
			resultErr = errors.Join(resultErr, result.err)
			continue
		}
		stored, err := s.catalog.PutObject(context.WithoutCancel(ctx), result.object)
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("catalog object %q: %w", result.object.RemoteID, err))
			continue
		}
		fetched++
		bytes += stored.Size
	}
	if err := ctx.Err(); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	return fetched, bytes, resultErr
}

func (s *Service) fetchObject(ctx context.Context, source objectSource, collection model.Collection, object remoteObject) objectFetchResult {
	body, contentType, err := source.Get(ctx, object)
	if err != nil {
		return objectFetchResult{err: fmt.Errorf("download object %q: %w", object.RemoteID, err)}
	}
	saved, saveErr := s.objectStore.Save(ctx, collection, object.RemoteID, object.ETag, contentType, object.Attributes, body)
	if err := errors.Join(saveErr, body.Close()); err != nil {
		return objectFetchResult{err: fmt.Errorf("store object %q: %w", object.RemoteID, err)}
	}
	return objectFetchResult{object: saved.Object}
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
		result.Error = s.cleanError(err)
		return result
	}
	result.UIDValidity = selectedMailbox.UIDValidity

	mailbox, err := s.catalog.EnsureMailbox(ctx, model.Mailbox{
		AccountID: account.ID, Name: remoteMailbox.Name, PathKey: mailstore.PathKey(remoteMailbox.Name), Delimiter: remoteMailbox.Delimiter,
		UIDValidity: selectedMailbox.UIDValidity, RemoteMessages: selectedMailbox.Messages,
	})
	if err != nil {
		result.Error = s.cleanError(err)
		return result
	}
	if _, err := s.store.PrepareMailbox(mailbox); err != nil {
		result.Error = s.cleanError(err)
		return result
	}

	uids, err := remote.SearchUIDsAfter(ctx, mailbox.LastUID)
	if err != nil {
		result.Error = s.cleanError(err)
		return result
	}
	result.Found = len(uids)
	for start := 0; start < len(uids); start += imapBatchSize {
		if err := ctx.Err(); err != nil {
			result.Error = s.cleanError(err)
			return result
		}
		end := min(start+imapBatchSize, len(uids))
		savedMessages := make([]mailstore.SavedMessage, 0, end-start)
		fetchErr := remote.FetchMessages(ctx, uids[start:end], func(fetched imapbackup.FetchedMessage, body io.Reader) error {
			saved, err := s.store.Save(ctx, mailbox, mailstore.FetchedMessage{UID: fetched.UID, InternalDate: fetched.InternalDate, Flags: fetched.Flags, ExpectedSize: fetched.Size, Body: body})
			if err == nil {
				savedMessages = append(savedMessages, saved)
			}
			return err
		})
		sort.Slice(savedMessages, func(left, right int) bool { return savedMessages[left].Message.UID < savedMessages[right].Message.UID })
		var catalogErr error
		for _, saved := range savedMessages {
			stored, err := s.catalog.PutMessage(context.WithoutCancel(ctx), saved.Message)
			if err != nil {
				catalogErr = errors.Join(catalogErr, err)
				continue
			}
			result.Fetched++
			result.Bytes += stored.Size
			s.logger.Debug("archived IMAP message", "account", account.ID, "mailbox", remoteMailbox.Name, "uid_validity", selectedMailbox.UIDValidity, "uid", saved.Message.UID, "bytes", stored.Size, "created", saved.Created)
		}
		if err := errors.Join(fetchErr, catalogErr); err != nil {
			result.Error = s.cleanError(err)
			return result
		}
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
