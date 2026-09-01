// Package jmap adapts rockorager's RFC 8620/8621 client for PIM Backup.
package jmap

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/core"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"
	"github.com/google/uuid"

	"github.com/lauritsk/backup/internal/pimbackup/config"
)

type Collection struct{ Name, RemoteID, Kind, URL string }
type Object struct {
	RemoteID, BlobID, ETag, ContentType, Title string
	Flags, MailboxIDs                          []string
	ReceivedAt                                 *time.Time
}

type Client struct {
	http            *http.Client
	client          *gojmap.Client
	accountID       gojmap.ID
	maxObjectsInGet int
}

// New uses account.URL when set and otherwise discovers the standard JMAP
// session resource at https://<host>/.well-known/jmap.
func New(ctx context.Context, account config.AccountConfig) (*Client, error) {
	endpoint, err := discoverEndpoint(account)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: account.InsecureSkipVerify}
	authenticated := &authTransport{next: transport, account: account, sessionEndpoint: endpoint, sessionContext: ctx}
	httpClient := &http.Client{Timeout: account.Timeout.Duration, Transport: authenticated}
	httpClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return errors.New("too many JMAP redirects")
		}
		if parsed.Scheme == "https" && request.URL.Scheme != "https" {
			return errors.New("refusing JMAP HTTPS downgrade")
		}
		return nil
	}
	library := &gojmap.Client{SessionEndpoint: endpoint, HttpClient: httpClient}
	if err := library.Authenticate(); err != nil {
		httpClient.CloseIdleConnections()
		return nil, fmt.Errorf("authenticate JMAP session: %w", err)
	}
	if _, supported := library.Session.RawCapabilities[mail.URI]; !supported {
		httpClient.CloseIdleConnections()
		return nil, errors.New("JMAP server does not advertise Mail capability")
	}
	accountID := library.Session.PrimaryAccounts[mail.URI]
	if accountID == "" && len(library.Session.Accounts) == 1 {
		for id := range library.Session.Accounts {
			accountID = id
		}
	}
	if accountID == "" {
		httpClient.CloseIdleConnections()
		return nil, errors.New("JMAP session has no primary Mail account")
	}
	maxObjects := 256
	if capability, ok := library.Session.Capabilities[gojmap.CoreURI].(*core.Core); ok && capability.MaxObjectsInGet > 0 {
		maxObjects = int(capability.MaxObjectsInGet)
	}
	select {
	case <-ctx.Done():
		httpClient.CloseIdleConnections()
		return nil, ctx.Err()
	default:
	}
	return &Client{http: httpClient, client: library, accountID: accountID, maxObjectsInGet: maxObjects}, nil
}

func discoverEndpoint(account config.AccountConfig) (string, error) {
	if account.URL != "" {
		return account.URL, nil
	}
	domain := account.Host
	if domain == "" {
		if index := strings.LastIndex(account.Username, "@"); index >= 0 {
			domain = account.Username[index+1:]
		}
	}
	if domain == "" {
		return "", errors.New("JMAP autodiscovery requires host or an email-style username")
	}
	return "https://" + domain + "/.well-known/jmap", nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Collection(ctx context.Context) (Collection, error) {
	response, err := c.do(ctx, &mailbox.Get{Account: c.accountID, Properties: []string{"id", "name"}})
	if err != nil {
		return Collection{}, err
	}
	if _, ok := response.(*mailbox.GetResponse); !ok {
		return Collection{}, errors.New("JMAP Mailbox/get returned an unexpected response")
	}
	return Collection{Name: "JMAP Mail", RemoteID: string(c.accountID), Kind: "mail", URL: c.client.SessionEndpoint}, nil
}

func (c *Client) Objects(ctx context.Context) ([]Object, string, error) {
	ids := make([]gojmap.ID, 0)
	position, queryState := int64(0), ""
	for {
		response, err := c.do(ctx, &email.Query{Account: c.accountID, Position: position, Limit: 500, CalculateTotal: true})
		if err != nil {
			return nil, "", err
		}
		page, ok := response.(*email.QueryResponse)
		if !ok {
			return nil, "", errors.New("JMAP Email/query returned an unexpected response")
		}
		if queryState != "" && page.QueryState != queryState {
			return nil, "", errors.New("JMAP query state changed while paginating; retry the backup")
		}
		queryState = page.QueryState
		ids = append(ids, page.IDs...)
		position += int64(len(page.IDs))
		if len(page.IDs) == 0 || page.Total > 0 && uint64(position) >= page.Total {
			break
		}
	}
	objects := make([]Object, 0, len(ids))
	for start := 0; start < len(ids); start += c.maxObjectsInGet {
		end := start + c.maxObjectsInGet
		if end > len(ids) {
			end = len(ids)
		}
		response, err := c.do(ctx, &email.Get{Account: c.accountID, IDs: ids[start:end], Properties: []string{"id", "blobId", "subject", "receivedAt", "keywords", "mailboxIds"}})
		if err != nil {
			return nil, "", err
		}
		page, ok := response.(*email.GetResponse)
		if !ok {
			return nil, "", errors.New("JMAP Email/get returned an unexpected response")
		}
		if len(page.NotFound) > 0 {
			return nil, "", errors.New("JMAP messages changed while fetching; retry the backup")
		}
		if len(page.List) != end-start {
			return nil, "", fmt.Errorf("JMAP Email/get returned %d of %d messages", len(page.List), end-start)
		}
		for _, item := range page.List {
			flags, mailboxIDs := trueKeys(item.Keywords), trueIDKeys(item.MailboxIDs)
			objects = append(objects, Object{RemoteID: string(item.ID), BlobID: string(item.BlobID), ETag: objectTag(string(item.BlobID), flags, mailboxIDs, item.ReceivedAt), ContentType: "message/rfc822", Title: item.Subject, Flags: flags, MailboxIDs: mailboxIDs, ReceivedAt: item.ReceivedAt})
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].RemoteID < objects[j].RemoteID })
	return objects, queryState, nil
}

func (c *Client) Get(ctx context.Context, object Object) (io.ReadCloser, error) {
	body, err := c.client.DownloadWithContext(ctx, c.accountID, gojmap.ID(object.BlobID))
	if err != nil {
		return nil, fmt.Errorf("download JMAP message: %w", err)
	}
	return body, nil
}

func (c *Client) Import(ctx context.Context, mailboxName string, flags []string, receivedAt *time.Time, body io.Reader) (string, error) {
	mailboxID, err := c.mailboxID(ctx, mailboxName)
	if err != nil {
		return "", err
	}
	uploaded, err := c.upload(ctx, body)
	if err != nil {
		return "", err
	}
	creationID := gojmap.ID(uuid.New().String())
	keywords := make(map[string]bool, len(flags))
	for _, flag := range flags {
		keywords[flag] = true
	}
	response, err := c.do(ctx, &email.Import{Account: c.accountID, Emails: map[string]*email.EmailImport{string(creationID): {BlobID: uploaded.ID, MailboxIDs: map[gojmap.ID]bool{mailboxID: true}, Keywords: keywords, ReceivedAt: receivedAt}}})
	if err != nil {
		return "", err
	}
	imported, ok := response.(*email.ImportResponse)
	if !ok {
		return "", errors.New("JMAP Email/import returned an unexpected response")
	}
	created := imported.Created[creationID]
	if created == nil || created.ID == "" {
		return "", errors.New("JMAP Email/import did not create the message")
	}
	return string(created.ID), nil
}

// go-jmap v0.5.3 sends application/json for every upload. Email/import
// requires a message/rfc822 blob, so use the session's advertised upload URL.
func (c *Client) upload(ctx context.Context, body io.Reader) (*gojmap.UploadResponse, error) {
	target := strings.ReplaceAll(c.client.Session.UploadURL, "{accountId}", url.PathEscape(string(c.accountID)))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, io.NopCloser(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "message/rfc822")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("upload JMAP message: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("upload JMAP message returned HTTP %d", response.StatusCode)
	}
	var uploaded gojmap.UploadResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&uploaded); err != nil {
		return nil, fmt.Errorf("decode JMAP upload: %w", err)
	}
	if uploaded.ID == "" {
		return nil, errors.New("JMAP upload returned no blob ID")
	}
	return &uploaded, nil
}

func (c *Client) mailboxID(ctx context.Context, name string) (gojmap.ID, error) {
	response, err := c.do(ctx, &mailbox.Get{Account: c.accountID, Properties: []string{"id", "name"}})
	if err != nil {
		return "", err
	}
	result, ok := response.(*mailbox.GetResponse)
	if !ok {
		return "", errors.New("JMAP Mailbox/get returned an unexpected response")
	}
	for _, item := range result.List {
		if item.Name == name {
			return item.ID, nil
		}
	}
	return "", fmt.Errorf("JMAP mailbox %q was not found", name)
}

func (c *Client) do(ctx context.Context, method gojmap.Method) (gojmap.MethodResponse, error) {
	request := &gojmap.Request{Context: ctx}
	request.Invoke(method)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("JMAP %s: %w", method.Name(), err)
	}
	if len(response.Responses) != 1 {
		return nil, fmt.Errorf("JMAP %s returned %d responses", method.Name(), len(response.Responses))
	}
	if response.Responses[0].Name == "error" {
		return nil, fmt.Errorf("JMAP %s returned a method error", method.Name())
	}
	methodResponse, ok := response.Responses[0].Args.(gojmap.MethodResponse)
	if !ok {
		return nil, fmt.Errorf("JMAP %s returned an unexpected response", method.Name())
	}
	return methodResponse, nil
}

func objectTag(blobID string, flags, mailboxIDs []string, receivedAt *time.Time) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, blobID+"\x00"+strings.Join(flags, "\x00")+"\x00"+strings.Join(mailboxIDs, "\x00"))
	if receivedAt != nil {
		_, _ = io.WriteString(hash, "\x00"+receivedAt.UTC().Format(time.RFC3339Nano))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
func trueKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for key, enabled := range values {
		if enabled {
			result = append(result, key)
		}
	}
	sort.Strings(result)
	return result
}
func trueIDKeys(values map[gojmap.ID]bool) []string {
	result := make([]string, 0, len(values))
	for key, enabled := range values {
		if enabled {
			result = append(result, string(key))
		}
	}
	sort.Strings(result)
	return result
}

type authTransport struct {
	next            http.RoundTripper
	account         config.AccountConfig
	sessionEndpoint string
	sessionContext  context.Context
}

func (t *authTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	requestContext := request.Context()
	if request.Method == http.MethodGet && request.URL.String() == t.sessionEndpoint {
		requestContext = t.sessionContext
	}
	clone := request.Clone(requestContext)
	clone.Header = request.Header.Clone()
	if t.account.Auth == "bearer" {
		clone.Header.Set("Authorization", "Bearer "+t.account.ResolvedToken)
	} else {
		clone.SetBasicAuth(t.account.Username, t.account.ResolvedPassword)
	}
	return t.next.RoundTrip(clone)
}
