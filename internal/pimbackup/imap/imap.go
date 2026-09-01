// Package imap provides the protocol-specific client used by PIM Backup.
package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/lauritsk/backup/internal/pimbackup/config"
)

type Mailbox struct {
	Name       string
	Delimiter  string
	Selectable bool
}

type SelectedMailbox struct {
	UIDValidity uint32
	UIDNext     uint32
	Messages    uint32
}

type FetchedMessage struct {
	UID          uint32
	InternalDate *time.Time
	Flags        []string
	Size         int64
}

type AppendResult struct {
	UID         uint32 `json:"uid,omitempty"`
	UIDValidity uint32 `json:"uid_validity,omitempty"`
}

type Remote interface {
	ListMailboxes(context.Context) ([]Mailbox, error)
	SelectMailbox(context.Context, string) (SelectedMailbox, error)
	SearchUIDsAfter(context.Context, uint32) ([]uint32, error)
	FetchMessage(context.Context, uint32, func(FetchedMessage, io.Reader) error) error
	EnsureMailbox(context.Context, string) error
	Append(context.Context, string, int64, []string, *time.Time, io.Reader) (AppendResult, error)
	Close() error
}

type Dialer interface {
	Dial(context.Context, config.AccountConfig) (Remote, error)
}

type NetworkDialer struct{}

func (NetworkDialer) Dial(ctx context.Context, account config.AccountConfig) (Remote, error) {
	address := net.JoinHostPort(account.Host, fmt.Sprintf("%d", account.Port))
	dialer := &net.Dialer{Timeout: account.Timeout.Duration}
	rawConnection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial IMAP account %q: %w", account.ID, err)
	}
	connection := &deadlineConn{Conn: rawConnection}
	if err := connection.setLimit(time.Now().Add(account.Timeout.Duration)); err != nil {
		connection.Close()
		return nil, fmt.Errorf("set IMAP authentication deadline for account %q: %w", account.ID, err)
	}

	tlsConfig := &tls.Config{
		ServerName:         account.Host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: account.InsecureSkipVerify,
		NextProtos:         []string{"imap"},
	}
	options := &imapclient.Options{TLSConfig: tlsConfig}
	var client *imapclient.Client
	switch account.TLS {
	case "implicit":
		tlsConnection := tls.Client(connection, tlsConfig)
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			connection.Close()
			return nil, fmt.Errorf("negotiate IMAP TLS for account %q: %w", account.ID, err)
		}
		client = imapclient.New(tlsConnection, options)
	case "starttls":
		client, err = imapclient.NewStartTLS(connection, options)
		if err != nil {
			connection.Close()
			return nil, fmt.Errorf("negotiate IMAP STARTTLS for account %q: %w", account.ID, err)
		}
	case "plain":
		client = imapclient.New(connection, options)
	default:
		connection.Close()
		return nil, fmt.Errorf("unsupported IMAP TLS mode %q", account.TLS)
	}

	remote := &clientRemote{
		client:      client,
		connection:  connection,
		timeout:     account.Timeout.Duration,
		contextDone: make(chan struct{}),
	}
	go remote.closeOnCancellation(ctx)
	if err := client.WaitGreeting(); err != nil {
		remote.abort()
		return nil, fmt.Errorf("read IMAP greeting for account %q: %w", account.ID, err)
	}
	if err := client.Login(account.Username, account.ResolvedPassword).Wait(); err != nil {
		remote.abort()
		return nil, fmt.Errorf("authenticate IMAP account %q: %w", account.ID, err)
	}
	if err := connection.clearLimit(); err != nil {
		remote.abort()
		return nil, fmt.Errorf("clear IMAP authentication deadline for account %q: %w", account.ID, err)
	}
	return remote, nil
}

type clientRemote struct {
	client      *imapclient.Client
	connection  *deadlineConn
	timeout     time.Duration
	contextDone chan struct{}
	closed      bool
}

func (r *clientRemote) closeOnCancellation(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = r.client.Close()
	case <-r.contextDone:
	}
}

func (r *clientRemote) abort() {
	if !r.closed {
		r.closed = true
		close(r.contextDone)
	}
	_ = r.client.Close()
}

func (r *clientRemote) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	close(r.contextDone)
	if err := r.connection.setLimit(time.Now().Add(r.timeout)); err != nil {
		_ = r.client.Close()
		return fmt.Errorf("set IMAP logout deadline: %w", err)
	}
	logoutErr := r.client.Logout().Wait()
	if logoutErr == nil {
		return nil
	}
	closeErr := r.client.Close()
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	}
	return errors.Join(logoutErr, closeErr)
}

func (r *clientRemote) operationDeadline(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(r.timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := r.connection.setLimit(deadline); err != nil {
		return nil, fmt.Errorf("set IMAP operation deadline: %w", err)
	}
	return func() { _ = r.connection.clearLimit() }, nil
}

type deadlineConn struct {
	net.Conn
	mutex sync.Mutex
	limit time.Time
}

func (c *deadlineConn) setLimit(limit time.Time) error {
	c.mutex.Lock()
	c.limit = limit
	c.mutex.Unlock()
	return c.Conn.SetDeadline(limit)
}

func (c *deadlineConn) clearLimit() error {
	c.mutex.Lock()
	c.limit = time.Time{}
	c.mutex.Unlock()
	return c.Conn.SetDeadline(time.Time{})
}

func (c *deadlineConn) SetDeadline(requested time.Time) error {
	deadline := c.capDeadline(requested)
	return c.Conn.SetDeadline(deadline)
}

func (c *deadlineConn) SetReadDeadline(requested time.Time) error {
	deadline := c.capDeadline(requested)
	return c.Conn.SetReadDeadline(deadline)
}

func (c *deadlineConn) SetWriteDeadline(requested time.Time) error {
	deadline := c.capDeadline(requested)
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *deadlineConn) capDeadline(requested time.Time) time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.limit.IsZero() {
		return requested
	}
	if requested.IsZero() || c.limit.Before(requested) {
		return c.limit
	}
	return requested
}

func (r *clientRemote) ListMailboxes(ctx context.Context) ([]Mailbox, error) {
	clearDeadline, err := r.operationDeadline(ctx)
	if err != nil {
		return nil, err
	}
	defer clearDeadline()
	listed, err := r.client.List("", "*", nil).Collect()
	if err != nil {
		return nil, fmt.Errorf("list IMAP mailboxes: %w", err)
	}
	mailboxes := make([]Mailbox, 0, len(listed))
	for _, entry := range listed {
		selectable := true
		for _, attribute := range entry.Attrs {
			if attribute == goimap.MailboxAttrNoSelect || attribute == goimap.MailboxAttrNonExistent {
				selectable = false
				break
			}
		}
		delimiter := ""
		if entry.Delim != 0 {
			delimiter = string(entry.Delim)
		}
		mailboxes = append(mailboxes, Mailbox{
			Name:       entry.Mailbox,
			Delimiter:  delimiter,
			Selectable: selectable,
		})
	}
	sort.Slice(mailboxes, func(i, j int) bool { return mailboxes[i].Name < mailboxes[j].Name })
	return mailboxes, nil
}

func (r *clientRemote) SelectMailbox(ctx context.Context, name string) (SelectedMailbox, error) {
	clearDeadline, err := r.operationDeadline(ctx)
	if err != nil {
		return SelectedMailbox{}, err
	}
	defer clearDeadline()
	selected, err := r.client.Select(name, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return SelectedMailbox{}, fmt.Errorf("select IMAP mailbox %q: %w", name, err)
	}
	if selected.UIDValidity == 0 {
		return SelectedMailbox{}, fmt.Errorf("select IMAP mailbox %q: server did not provide UIDVALIDITY", name)
	}
	return SelectedMailbox{
		UIDValidity: selected.UIDValidity,
		UIDNext:     uint32(selected.UIDNext),
		Messages:    selected.NumMessages,
	}, nil
}

func (r *clientRemote) SearchUIDsAfter(ctx context.Context, after uint32) ([]uint32, error) {
	clearDeadline, err := r.operationDeadline(ctx)
	if err != nil {
		return nil, err
	}
	defer clearDeadline()
	if after == ^uint32(0) {
		return nil, nil
	}
	set := goimap.UIDSet{}
	set.AddRange(goimap.UID(after+1), 0)
	result, err := r.client.UIDSearch(&goimap.SearchCriteria{UID: []goimap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search IMAP UIDs after %d: %w", after, err)
	}
	uids := result.AllUIDs()
	out := make([]uint32, len(uids))
	for i, uid := range uids {
		out[i] = uint32(uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (r *clientRemote) FetchMessage(ctx context.Context, uid uint32, consume func(FetchedMessage, io.Reader) error) error {
	clearDeadline, err := r.operationDeadline(ctx)
	if err != nil {
		return err
	}
	defer clearDeadline()
	bodySection := &goimap.FetchItemBodySection{Peek: true}
	command := r.client.Fetch(goimap.UIDSetNum(goimap.UID(uid)), &goimap.FetchOptions{
		UID:          true,
		Flags:        true,
		InternalDate: true,
		RFC822Size:   true,
		BodySection:  []*goimap.FetchItemBodySection{bodySection},
	})

	closed := false
	defer func() {
		if !closed {
			_ = command.Close()
		}
	}()

	data := command.Next()
	if data == nil {
		err := command.Close()
		closed = true
		if err != nil {
			return fmt.Errorf("fetch IMAP UID %d: %w", uid, err)
		}
		return fmt.Errorf("fetch IMAP UID %d: server returned no message", uid)
	}
	fetched := FetchedMessage{Size: -1}
	bodySeen := false
	for {
		item := data.Next()
		if item == nil {
			break
		}
		switch item := item.(type) {
		case imapclient.FetchItemDataUID:
			fetched.UID = uint32(item.UID)
		case imapclient.FetchItemDataFlags:
			fetched.Flags = make([]string, len(item.Flags))
			for i, flag := range item.Flags {
				fetched.Flags[i] = string(flag)
			}
		case imapclient.FetchItemDataInternalDate:
			value := item.Time.UTC()
			fetched.InternalDate = &value
		case imapclient.FetchItemDataRFC822Size:
			fetched.Size = item.Size
		case imapclient.FetchItemDataBodySection:
			if bodySeen {
				return fmt.Errorf("fetch IMAP UID %d: server returned multiple bodies", uid)
			}
			if item.Literal == nil {
				return fmt.Errorf("fetch IMAP UID %d: server returned an empty body item", uid)
			}
			if fetched.UID != uid {
				return fmt.Errorf("fetch IMAP UID %d: server returned UID %d", uid, fetched.UID)
			}
			if fetched.Size < 0 {
				return fmt.Errorf("fetch IMAP UID %d: server did not return RFC822.SIZE", uid)
			}
			bodySeen = true
			if err := consume(fetched, item.Literal); err != nil {
				return err
			}
		}
	}
	if !bodySeen {
		return fmt.Errorf("fetch IMAP UID %d: server did not return the message body", uid)
	}
	if extra := command.Next(); extra != nil {
		return fmt.Errorf("fetch IMAP UID %d: server returned more than one message", uid)
	}
	if err := command.Close(); err != nil {
		closed = true
		return fmt.Errorf("fetch IMAP UID %d: %w", uid, err)
	}
	closed = true
	return nil
}

func (r *clientRemote) EnsureMailbox(ctx context.Context, mailbox string) error {
	mailboxes, err := r.ListMailboxes(ctx)
	if err != nil {
		return err
	}
	for _, existing := range mailboxes {
		if existing.Name != mailbox {
			continue
		}
		if !existing.Selectable {
			return fmt.Errorf("IMAP mailbox %q exists but cannot accept messages", mailbox)
		}
		return nil
	}
	clearDeadline, err := r.operationDeadline(ctx)
	if err != nil {
		return err
	}
	defer clearDeadline()
	if err := r.client.Create(mailbox, nil).Wait(); err != nil {
		return fmt.Errorf("create IMAP mailbox %q: %w", mailbox, err)
	}
	return nil
}

func (r *clientRemote) Append(ctx context.Context, mailbox string, size int64, flags []string, internalDate *time.Time, body io.Reader) (AppendResult, error) {
	clearDeadline, err := r.operationDeadline(ctx)
	if err != nil {
		return AppendResult{}, err
	}
	defer clearDeadline()
	options := &goimap.AppendOptions{Flags: make([]goimap.Flag, len(flags))}
	for i, flag := range flags {
		options.Flags[i] = goimap.Flag(flag)
	}
	if internalDate != nil {
		options.Time = *internalDate
	}
	command := r.client.Append(mailbox, size, options)
	if _, err := io.Copy(command, body); err != nil {
		_ = command.Close()
		return AppendResult{}, fmt.Errorf("write IMAP APPEND for mailbox %q: %w", mailbox, err)
	}
	if err := command.Close(); err != nil {
		return AppendResult{}, fmt.Errorf("finish IMAP APPEND for mailbox %q: %w", mailbox, err)
	}
	result, err := command.Wait()
	if err != nil {
		return AppendResult{}, fmt.Errorf("append message to IMAP mailbox %q: %w", mailbox, err)
	}
	return AppendResult{UID: uint32(result.UID), UIDValidity: result.UIDValidity}, nil
}
