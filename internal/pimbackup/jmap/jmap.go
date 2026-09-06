// Package jmap adapts rockorager's RFC 8620/8621 client for PIM Backup.
package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/core"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"
	"uuid"

	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/tlsconfig"
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
	if account.InsecureSkipVerify && !account.AllowInsecure {
		return nil, errors.New("insecure_skip_verify requires allow_insecure = true")
	}
	endpoint, err := discoverEndpoint(account)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "https" && !account.AllowInsecure {
		return nil, errors.New("an HTTP url requires allow_insecure = true")
	}
	tlsClientConfig, err := tlsconfig.Client("", account.CAFile, account.InsecureSkipVerify)
	if err != nil {
		return nil, fmt.Errorf("configure JMAP TLS: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsClientConfig
	authenticated, err := newAuthTransport(transport, account, endpoint, ctx)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: account.Timeout.Duration, Transport: authenticated}
	httpClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return errors.New("too many JMAP redirects")
		}
		if parsed.Scheme == "https" && request.URL.Scheme != "https" {
			return errors.New("refusing JMAP HTTPS downgrade")
		}
		if !authenticated.allowedURL(request.URL) {
			return errors.New("refusing JMAP redirect to an unapproved origin")
		}
		return nil
	}
	library := &gojmap.Client{SessionEndpoint: endpoint, HttpClient: httpClient}
	if err := library.Authenticate(); err != nil {
		httpClient.CloseIdleConnections()
		return nil, fmt.Errorf("authenticate JMAP session: %w", err)
	}
	if err := authenticated.allowURLs(library.Session.APIURL, library.Session.DownloadURL, library.Session.UploadURL); err != nil {
		httpClient.CloseIdleConnections()
		return nil, err
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

// Objects returns a full Email/query snapshot on the first run. Later runs use
// Email/changes with the Email state committed by the previous run.
func (c *Client) Objects(ctx context.Context, sinceState string) ([]Object, string, error) {
	if sinceState != "" {
		objects, state, err := c.changedObjects(ctx, sinceState)
		var methodErr *gojmap.MethodError
		if err == nil || !errors.As(err, &methodErr) || methodErr.Type != "cannotCalculateChanges" {
			return objects, state, err
		}
		// Servers may discard old states. Rebuild a stable snapshot rather than
		// leaving this account permanently stuck on an expired cursor.
	}
	return c.allObjects(ctx)
}

func (c *Client) changedObjects(ctx context.Context, sinceState string) ([]Object, string, error) {
	ids := make(map[gojmap.ID]struct{})
	state := sinceState
	for {
		response, err := c.do(ctx, &email.Changes{Account: c.accountID, SinceState: state, MaxChanges: 500})
		if err != nil {
			return nil, "", err
		}
		changes, ok := response.(*email.ChangesResponse)
		if !ok {
			return nil, "", errors.New("JMAP Email/changes returned an unexpected response")
		}
		if changes.OldState != "" && changes.OldState != state {
			return nil, "", errors.New("JMAP Email/changes returned the wrong old state")
		}
		for _, id := range changes.Created {
			ids[id] = struct{}{}
		}
		for _, id := range changes.Updated {
			ids[id] = struct{}{}
		}
		for _, id := range changes.Destroyed {
			delete(ids, id)
		}
		if changes.NewState == "" {
			return nil, "", errors.New("JMAP Email/changes returned an empty state")
		}
		state = changes.NewState
		if !changes.HasMoreChanges {
			break
		}
	}
	changed := make([]gojmap.ID, 0, len(ids))
	for id := range ids {
		changed = append(changed, id)
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i] < changed[j] })
	objects, fetchedState, err := c.getObjects(ctx, changed)
	if err != nil {
		return nil, "", err
	}
	if len(changed) > 0 && fetchedState != state {
		return nil, "", errors.New("JMAP Email state changed after Email/changes; retry the backup")
	}
	return objects, state, nil
}

func (c *Client) allObjects(ctx context.Context) ([]Object, string, error) {
	ids, _, err := c.queryIDs(ctx)
	if err != nil {
		return nil, "", err
	}
	var objects []Object
	var state string
	if len(ids) == 0 {
		objects, state, err = c.emptySnapshot(ctx)
	} else {
		objects, state, err = c.getObjects(ctx, ids)
	}
	if err != nil {
		return nil, "", err
	}
	confirmed, _, err := c.queryIDs(ctx)
	if err != nil {
		return nil, "", err
	}
	if !slices.Equal(ids, confirmed) {
		return nil, "", errors.New("JMAP Email/query changed while building the initial snapshot; retry the backup")
	}
	return objects, state, nil
}

func (c *Client) queryIDs(ctx context.Context) ([]gojmap.ID, string, error) {
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
	return ids, queryState, nil
}

func (c *Client) emptySnapshot(ctx context.Context) ([]Object, string, error) {
	// Email/get with one impossible local sentinel obtains the Email state
	// without asking the server to return every message.
	response, err := c.do(ctx, &email.Get{Account: c.accountID, IDs: []gojmap.ID{"pimbackup-invalid-id"}, Properties: []string{"id"}})
	if err != nil {
		return nil, "", err
	}
	page, ok := response.(*email.GetResponse)
	if !ok || page.State == "" {
		return nil, "", errors.New("JMAP Email/get returned no state")
	}
	return []Object{}, page.State, nil
}

func (c *Client) getObjects(ctx context.Context, ids []gojmap.ID) ([]Object, string, error) {
	objects := make([]Object, 0, len(ids))
	state := ""
	for start := 0; start < len(ids); start += c.maxObjectsInGet {
		end := min(start+c.maxObjectsInGet, len(ids))
		response, err := c.do(ctx, &email.Get{Account: c.accountID, IDs: ids[start:end], Properties: []string{"id", "blobId", "subject", "receivedAt", "keywords", "mailboxIds"}})
		if err != nil {
			return nil, "", err
		}
		page, ok := response.(*email.GetResponse)
		if !ok {
			return nil, "", errors.New("JMAP Email/get returned an unexpected response")
		}
		if len(page.NotFound) > 0 || len(page.List) != end-start {
			return nil, "", errors.New("JMAP messages changed while fetching; retry the backup")
		}
		if state != "" && page.State != state {
			return nil, "", errors.New("JMAP Email state changed while fetching; retry the backup")
		}
		state = page.State
		for _, item := range page.List {
			flags, mailboxIDs := trueKeys(item.Keywords), trueIDKeys(item.MailboxIDs)
			objects = append(objects, Object{RemoteID: string(item.ID), BlobID: string(item.BlobID), ETag: objectTag(string(item.BlobID), flags, mailboxIDs, item.ReceivedAt), ContentType: "message/rfc822", Title: item.Subject, Flags: flags, MailboxIDs: mailboxIDs, ReceivedAt: item.ReceivedAt})
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].RemoteID < objects[j].RemoteID })
	return objects, state, nil
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
		if methodErr, ok := response.Responses[0].Args.(*gojmap.MethodError); ok {
			return nil, fmt.Errorf("JMAP %s: %w", method.Name(), methodErr)
		}
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
	mutex           sync.RWMutex
	origins         map[string]struct{}
}

func newAuthTransport(next http.RoundTripper, account config.AccountConfig, endpoint string, sessionContext context.Context) (*authTransport, error) {
	transport := &authTransport{next: next, account: account, sessionEndpoint: endpoint, sessionContext: sessionContext, origins: make(map[string]struct{})}
	if err := transport.allowURLs(endpoint); err != nil {
		return nil, fmt.Errorf("approve JMAP session origin: %w", err)
	}
	return transport, nil
}

func (t *authTransport) allowURLs(values ...string) error {
	session, err := url.Parse(t.sessionEndpoint)
	if err != nil {
		return errors.New("invalid JMAP session URL")
	}
	origins := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
			return errors.New("JMAP session advertised an invalid service URL")
		}
		if session.Scheme == "https" && parsed.Scheme != "https" {
			return errors.New("refusing JMAP HTTPS downgrade")
		}
		origins = append(origins, strings.ToLower(parsed.Scheme+"://"+parsed.Host))
	}
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for _, origin := range origins {
		t.origins[origin] = struct{}{}
	}
	return nil
}

func (t *authTransport) allowedURL(value *url.URL) bool {
	if value == nil || value.User != nil {
		return false
	}
	origin := strings.ToLower(value.Scheme + "://" + value.Host)
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	_, allowed := t.origins[origin]
	return allowed
}

func (t *authTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !t.allowedURL(request.URL) {
		return nil, errors.New("refusing to send JMAP credentials to an unapproved origin")
	}
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
