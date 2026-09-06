package pimbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/pimbackup/dav"
	imapbackup "github.com/lauritsk/backup/internal/pimbackup/imap"
	"github.com/lauritsk/backup/internal/pimbackup/jmap"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

type messageDestination interface {
	Put(context.Context, model.Message, io.Reader) (model.RestoredMessage, error)
	Close() error
}

type objectDestination interface {
	Put(context.Context, model.Object, io.Reader) (model.RestoredObject, error)
	Close() error
}

type imapMessageDestination struct {
	remote  imapbackup.Remote
	mailbox string
}

type jmapMessageDestination struct {
	client  *jmap.Client
	mailbox string
}

type imapObjectDestination struct {
	remote  imapbackup.Remote
	mailbox string
}

type jmapObjectDestination struct {
	client  *jmap.Client
	mailbox string
}

type davObjectDestination struct {
	client       *dav.Client
	collection   string
	kind         string
	kindPlural   string
	protocolName string
}

func (s *Service) restore(ctx context.Context, request model.RestoreRequest) (model.RestoreReport, error) {
	report := model.RestoreReport{TargetAccount: request.TargetAccount, TargetMailbox: request.TargetMailbox, TargetCollection: request.TargetCollection}
	if !request.Confirm {
		return report, errors.New("restore requires explicit confirmation")
	}
	if len(request.MessageIDs) > 0 && len(request.ObjectIDs) > 0 {
		return report, errors.New("restore accepts message IDs or object IDs, not both")
	}
	if len(request.ObjectIDs) > 0 {
		return s.restoreObjects(ctx, request)
	}
	if len(request.MessageIDs) == 0 {
		return report, errors.New("restore requires at least one message or object ID")
	}
	return s.restoreMessages(ctx, request)
}

func (s *Service) restoreMessages(ctx context.Context, request model.RestoreRequest) (report model.RestoreReport, resultErr error) {
	report = model.RestoreReport{TargetAccount: request.TargetAccount, TargetMailbox: request.TargetMailbox}
	if len(request.MessageIDs) > 1000 {
		return report, errors.New("restore accepts at most 1000 message IDs per run")
	}
	if request.TargetAccount == "" || request.TargetMailbox == "" {
		return report, errors.New("restore requires target_account and target_mailbox")
	}
	account, found := s.config.Account(request.TargetAccount)
	if !found || account.Disabled {
		return report, fmt.Errorf("target account %q is not configured and enabled", request.TargetAccount)
	}
	destination, err := s.newMessageDestination(ctx, request, account)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, destination.Close()) }()

	for _, id := range uniqueIDs(request.MessageIDs) {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		entry := model.RestoredMessage{MessageID: id}
		_, restoreErr := s.withVerifiedMessage(ctx, id, func(message model.Message, body io.Reader) error {
			var putErr error
			entry, putErr = destination.Put(ctx, message, body)
			entry.MessageID = id
			return putErr
		})
		if restoreErr != nil {
			entry.Error = s.cleanError(restoreErr)
			report.Failed++
		} else {
			report.Restored++
		}
		report.Messages = append(report.Messages, entry)
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("restore completed with %d error(s)", report.Failed)
	}
	return report, nil
}

func (s *Service) restoreObjects(ctx context.Context, request model.RestoreRequest) (report model.RestoreReport, resultErr error) {
	report = model.RestoreReport{TargetAccount: request.TargetAccount, TargetMailbox: request.TargetMailbox, TargetCollection: request.TargetCollection}
	if len(request.ObjectIDs) > 1000 {
		return report, errors.New("restore accepts at most 1000 object IDs per run")
	}
	account, found := s.config.Account(request.TargetAccount)
	if !found || account.Disabled {
		return report, fmt.Errorf("target account %q is not configured and enabled", request.TargetAccount)
	}
	destination, err := s.newObjectDestination(ctx, request, account)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, destination.Close()) }()

	for _, id := range uniqueIDs(request.ObjectIDs) {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		entry := model.RestoredObject{ObjectID: id}
		_, restoreErr := s.withVerifiedObject(ctx, id, func(object model.Object, body io.Reader) error {
			var putErr error
			entry, putErr = destination.Put(ctx, object, body)
			entry.ObjectID = id
			return putErr
		})
		if restoreErr != nil {
			entry.Error = s.cleanError(restoreErr)
			report.Failed++
		} else {
			report.Restored++
		}
		report.Objects = append(report.Objects, entry)
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("restore completed with %d error(s)", report.Failed)
	}
	return report, nil
}

func (s *Service) newMessageDestination(ctx context.Context, request model.RestoreRequest, account config.AccountConfig) (messageDestination, error) {
	switch account.Protocol {
	case "imap":
		if account.ResolvedPassword == "" {
			return nil, fmt.Errorf("target account %q has no password", request.TargetAccount)
		}
		remote, err := s.dialer.Dial(ctx, account)
		if err != nil {
			return nil, err
		}
		if request.CreateMailbox {
			if err := remote.EnsureMailbox(ctx, request.TargetMailbox); err != nil {
				return nil, errors.Join(err, remote.Close())
			}
		}
		return &imapMessageDestination{remote: remote, mailbox: request.TargetMailbox}, nil
	case "jmap":
		client, err := jmap.New(ctx, account)
		if err != nil {
			return nil, err
		}
		return &jmapMessageDestination{client: client, mailbox: request.TargetMailbox}, nil
	default:
		return nil, fmt.Errorf("message restore target %q is not a mail account", request.TargetAccount)
	}
}

func (s *Service) newObjectDestination(ctx context.Context, request model.RestoreRequest, account config.AccountConfig) (objectDestination, error) {
	switch account.Protocol {
	case "imap":
		if request.TargetMailbox == "" {
			return nil, errors.New("IMAP restore requires target_mailbox")
		}
		remote, err := s.dialer.Dial(ctx, account)
		if err != nil {
			return nil, err
		}
		if request.CreateMailbox {
			if err := remote.EnsureMailbox(ctx, request.TargetMailbox); err != nil {
				return nil, errors.Join(err, remote.Close())
			}
		}
		return &imapObjectDestination{remote: remote, mailbox: request.TargetMailbox}, nil
	case "jmap":
		if request.TargetMailbox == "" {
			return nil, errors.New("JMAP restore requires target_mailbox")
		}
		client, err := jmap.New(ctx, account)
		if err != nil {
			return nil, err
		}
		return &jmapObjectDestination{client: client, mailbox: request.TargetMailbox}, nil
	case "carddav", "caldav":
		if request.TargetCollection == "" {
			return nil, errors.New("DAV restore requires target_collection")
		}
		client, err := dav.New(ctx, account)
		if err != nil {
			return nil, err
		}
		collections, err := client.Collections(ctx)
		if err != nil {
			client.Close()
			return nil, err
		}
		for _, collection := range collections {
			if collection.Name == request.TargetCollection || collection.RemoteID == request.TargetCollection {
				kind, plural, protocolName := "contact", "contacts", "CardDAV"
				if account.Protocol == "caldav" {
					kind, plural, protocolName = "calendar", "calendars", "CalDAV"
				}
				return &davObjectDestination{client: client, collection: collection.URL, kind: kind, kindPlural: plural, protocolName: protocolName}, nil
			}
		}
		client.Close()
		return nil, fmt.Errorf("DAV collection %q was not found", request.TargetCollection)
	default:
		return nil, fmt.Errorf("object restore target %q does not support standard objects", request.TargetAccount)
	}
}

func (destination *imapMessageDestination) Put(ctx context.Context, message model.Message, body io.Reader) (model.RestoredMessage, error) {
	appended, err := destination.remote.Append(ctx, destination.mailbox, message.Size, restorableFlags(message.Flags), message.InternalDate, body)
	return model.RestoredMessage{UID: appended.UID, UIDValidity: appended.UIDValidity}, err
}

func (destination *imapMessageDestination) Close() error { return destination.remote.Close() }

func (destination *jmapMessageDestination) Put(ctx context.Context, message model.Message, body io.Reader) (model.RestoredMessage, error) {
	remoteID, err := destination.client.Import(ctx, destination.mailbox, imapFlagsToJMAP(message.Flags), message.InternalDate, body)
	return model.RestoredMessage{RemoteID: remoteID}, err
}

func (destination *jmapMessageDestination) Close() error {
	destination.client.Close()
	return nil
}

func (destination *imapObjectDestination) Put(ctx context.Context, object model.Object, body io.Reader) (model.RestoredObject, error) {
	if object.Kind != "mail" {
		return model.RestoredObject{}, errors.New("only mail objects can be restored to IMAP")
	}
	appended, err := destination.remote.Append(ctx, destination.mailbox, object.Size, jmapFlagsToIMAP(object.Flags), object.InternalDate, body)
	return model.RestoredObject{UID: appended.UID, UIDValidity: appended.UIDValidity}, err
}

func (destination *imapObjectDestination) Close() error { return destination.remote.Close() }

func (destination *jmapObjectDestination) Put(ctx context.Context, object model.Object, body io.Reader) (model.RestoredObject, error) {
	if object.Kind != "mail" {
		return model.RestoredObject{}, errors.New("only mail objects can be restored to JMAP")
	}
	remoteID, err := destination.client.Import(ctx, destination.mailbox, object.Flags, object.InternalDate, body)
	return model.RestoredObject{RemoteID: remoteID}, err
}

func (destination *jmapObjectDestination) Close() error {
	destination.client.Close()
	return nil
}

func (destination *davObjectDestination) Put(ctx context.Context, object model.Object, body io.Reader) (model.RestoredObject, error) {
	if object.Kind != destination.kind {
		return model.RestoredObject{}, fmt.Errorf("only %s can be restored to %s", destination.kindPlural, destination.protocolName)
	}
	remoteID, err := destination.client.Put(ctx, destination.collection, object.Kind, object.ContentType, body)
	return model.RestoredObject{RemoteID: remoteID}, err
}

func (destination *davObjectDestination) Close() error {
	destination.client.Close()
	return nil
}

func (s *Service) withVerifiedMessage(ctx context.Context, id int64, action func(model.Message, io.Reader) error) (model.Message, error) {
	message, err := s.catalog.GetMessage(ctx, id)
	if err != nil {
		return model.Message{}, err
	}
	if err := s.store.VerifyIntegrity(ctx, message); err != nil {
		return message, err
	}
	file, err := s.store.OpenMessage(message)
	if err != nil {
		return message, err
	}
	return message, errors.Join(action(message, file), file.Close())
}

func (s *Service) withVerifiedObject(ctx context.Context, id int64, action func(model.Object, io.Reader) error) (model.Object, error) {
	object, err := s.catalog.GetObject(ctx, id)
	if err != nil {
		return model.Object{}, err
	}
	if err := s.objectStore.Verify(ctx, object); err != nil {
		return object, err
	}
	file, err := s.objectStore.Open(object)
	if err != nil {
		return object, err
	}
	return object, errors.Join(action(object, file), file.Close())
}

func uniqueIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func imapFlagsToJMAP(flags []string) []string {
	mapping := map[string]string{"\\seen": "$seen", "\\flagged": "$flagged", "\\answered": "$answered", "\\draft": "$draft"}
	result := make([]string, 0, len(flags))
	for _, flag := range flags {
		if keyword, found := mapping[strings.ToLower(flag)]; found {
			result = append(result, keyword)
		} else if !strings.HasPrefix(flag, "\\") {
			result = append(result, flag)
		}
	}
	sort.Strings(result)
	return result
}

func jmapFlagsToIMAP(flags []string) []string {
	mapping := map[string]string{"$seen": "\\Seen", "$flagged": "\\Flagged", "$answered": "\\Answered", "$draft": "\\Draft"}
	result := make([]string, 0, len(flags))
	for _, flag := range flags {
		if imapFlag, found := mapping[strings.ToLower(flag)]; found {
			result = append(result, imapFlag)
		} else if !strings.HasPrefix(flag, "$") {
			result = append(result, flag)
		}
	}
	return result
}

func restorableFlags(flags []string) []string {
	result := make([]string, 0, len(flags))
	for _, flag := range flags {
		if strings.EqualFold(flag, "\\Recent") || strings.EqualFold(flag, "\\Deleted") {
			continue
		}
		result = append(result, flag)
	}
	return result
}
