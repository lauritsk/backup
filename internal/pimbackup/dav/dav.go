// Package dav adapts emersion's WebDAV clients for CardDAV and CalDAV backup.
package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
	"github.com/emersion/go-webdav/carddav"
	"uuid"

	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/tlsconfig"
)

var (
	discoverCardDAV = carddav.DiscoverContextURL
	discoverCalDAV  = caldav.DiscoverContextURL
)

type Collection struct{ Name, RemoteID, URL, SyncToken, Kind string }
type Object struct{ RemoteID, URL, ETag, ContentType string }

type Client struct {
	account  config.AccountConfig
	endpoint *url.URL
	http     *http.Client
	webdav   *webdav.Client
	carddav  *carddav.Client
	caldav   *caldav.Client
}

// New discovers an endpoint when account.URL is empty. An explicit URL is an
// override and may point at a service root, collection home, or collection.
func New(ctx context.Context, account config.AccountConfig) (*Client, error) {
	if account.InsecureSkipVerify && !account.AllowInsecure {
		return nil, errors.New("insecure_skip_verify requires allow_insecure = true")
	}
	endpoint, err := discoverEndpoint(ctx, account)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse DAV endpoint: %w", err)
	}
	if parsed.Scheme != "https" && !account.AllowInsecure {
		return nil, errors.New("an HTTP url requires allow_insecure = true")
	}
	tlsClientConfig, err := tlsconfig.Client("", account.CAFile, account.InsecureSkipVerify)
	if err != nil {
		return nil, fmt.Errorf("configure DAV TLS: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsClientConfig
	httpClient := &http.Client{Timeout: account.Timeout.Duration, Transport: transport}
	httpClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 0 && via[0].Method == http.MethodPut {
			return errors.New("refusing DAV PUT redirect")
		}
		if len(via) > 10 {
			return errors.New("too many DAV redirects")
		}
		if parsed.Scheme == "https" && request.URL.Scheme != "https" {
			return errors.New("refusing DAV HTTPS downgrade")
		}
		if !sameOrigin(parsed, request.URL) {
			return errors.New("refusing DAV redirect to another origin")
		}
		setAuth(request, account)
		return nil
	}
	authenticated := authenticatedClient{client: httpClient, account: account, origin: parsed}
	wc, err := webdav.NewClient(authenticated, endpoint)
	if err != nil {
		return nil, err
	}
	client := &Client{account: account, endpoint: parsed, http: httpClient, webdav: wc}
	switch account.Protocol {
	case "carddav":
		client.carddav, err = carddav.NewClient(authenticated, endpoint)
	case "caldav":
		client.caldav, err = caldav.NewClient(authenticated, endpoint)
	default:
		err = fmt.Errorf("unsupported DAV protocol %q", account.Protocol)
	}
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, err
	}
	return client, nil
}

func discoverEndpoint(ctx context.Context, account config.AccountConfig) (string, error) {
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
		return "", errors.New("DAV autodiscovery requires host or an email-style username")
	}
	var endpoint string
	var err error
	if account.Protocol == "carddav" {
		endpoint, err = discoverCardDAV(ctx, domain)
	} else {
		endpoint, err = discoverCalDAV(ctx, domain)
	}
	if err == nil {
		return endpoint, nil
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsTemporary {
		return "", fmt.Errorf("temporary DAV discovery failure: %w", err)
	}
	return "https://" + domain + "/.well-known/" + account.Protocol, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) Collections(ctx context.Context) ([]Collection, error) {
	var result []Collection
	var discoveryErr error
	if c.account.URL != "" {
		switch c.account.Protocol {
		case "carddav":
			books, err := c.carddav.FindAddressBooks(ctx, c.endpoint.Path)
			discoveryErr = err
			if err == nil {
				for _, book := range books {
					result = append(result, Collection{Name: collectionName(book.Name, book.Path, "Contacts"), RemoteID: book.Path, URL: c.resolve(book.Path), Kind: "contact"})
				}
			}
		case "caldav":
			calendars, err := c.caldav.FindCalendars(ctx, c.endpoint.Path)
			discoveryErr = err
			if err == nil {
				for _, calendar := range calendars {
					result = append(result, Collection{Name: collectionName(calendar.Name, calendar.Path, "Calendar"), RemoteID: calendar.Path, URL: c.resolve(calendar.Path), Kind: "calendar"})
				}
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	switch c.account.Protocol {
	case "carddav":
		principal, err := c.carddav.FindCurrentUserPrincipal(ctx)
		if err == nil {
			var home string
			home, err = c.carddav.FindAddressBookHomeSet(ctx, principal)
			if err == nil {
				var books []carddav.AddressBook
				books, err = c.carddav.FindAddressBooks(ctx, home)
				if err == nil {
					for _, book := range books {
						result = append(result, Collection{Name: collectionName(book.Name, book.Path, "Contacts"), RemoteID: book.Path, URL: c.resolve(book.Path), Kind: "contact"})
					}
				}
			}
		}
		discoveryErr = err
	case "caldav":
		principal, err := c.caldav.FindCurrentUserPrincipal(ctx)
		if err == nil {
			var home string
			home, err = c.caldav.FindCalendarHomeSet(ctx, principal)
			if err == nil {
				var calendars []caldav.Calendar
				calendars, err = c.caldav.FindCalendars(ctx, home)
				if err == nil {
					for _, calendar := range calendars {
						result = append(result, Collection{Name: collectionName(calendar.Name, calendar.Path, "Calendar"), RemoteID: calendar.Path, URL: c.resolve(calendar.Path), Kind: "calendar"})
					}
				}
			}
		}
		discoveryErr = err
	}
	if len(result) > 0 {
		return result, nil
	}
	// Explicit collection URLs are useful for servers with incomplete
	// principal discovery. Autodiscovered endpoints must pass discovery.
	if c.account.URL == "" {
		return nil, fmt.Errorf("discover %s collections: %w", c.account.Protocol, discoveryErr)
	}
	collectionPath := c.endpoint.Path
	if collectionPath == "" {
		collectionPath = "/"
	}
	if _, err := c.webdav.Stat(ctx, collectionPath); err != nil {
		return nil, errors.Join(discoveryErr, fmt.Errorf("inspect explicit DAV collection: %w", err))
	}
	kind, fallback := "contact", "Contacts"
	if c.account.Protocol == "caldav" {
		kind, fallback = "calendar", "Calendar"
	}
	return []Collection{{Name: collectionName("", collectionPath, fallback), RemoteID: collectionPath, URL: c.resolve(collectionPath), Kind: kind}}, nil
}

func (c *Client) Objects(ctx context.Context, collection Collection) ([]Object, string, error) {
	objects, token, supported, err := c.syncCollection(ctx, collection)
	if err != nil {
		return nil, "", err
	}
	if supported {
		return objects, token, nil
	}
	entries, err := c.webdav.ReadDir(ctx, collection.RemoteID, false)
	if err != nil {
		return nil, "", err
	}
	result := make([]Object, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || samePath(entry.Path, collection.RemoteID) {
			continue
		}
		result = append(result, Object{RemoteID: entry.Path, URL: c.resolve(entry.Path), ETag: entry.ETag, ContentType: entry.MIMEType})
	}
	return result, "", nil
}

// syncCollection uses RFC 6578 directly for both CardDAV and CalDAV. The
// go-webdav release used here exposes this operation only through CardDAV.
func (c *Client) syncCollection(ctx context.Context, collection Collection) ([]Object, string, bool, error) {
	target, err := url.Parse(collection.URL)
	if err != nil || !sameOrigin(c.endpoint, target) {
		return nil, "", false, errors.New("refusing to send DAV credentials to another origin")
	}
	body := `<?xml version="1.0" encoding="utf-8"?><d:sync-collection xmlns:d="DAV:"><d:sync-token>` +
		xmlEscape(collection.SyncToken) + `</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/><d:getcontenttype/></d:prop></d:sync-collection>`
	request, err := http.NewRequestWithContext(ctx, "REPORT", target.String(), bytes.NewBufferString(body))
	if err != nil {
		return nil, "", false, err
	}
	request.Header.Set("Content-Type", "application/xml; charset=utf-8")
	request.Header.Set("Depth", "1")
	setAuth(request, c.account)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMultiStatus {
		// Unsupported reports and expired tokens both require a full inventory.
		// Do not advance the token on that fallback.
		if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusNotImplemented {
			return nil, "", false, nil
		}
		return nil, "", false, fmt.Errorf("DAV sync-collection returned HTTP %d", response.StatusCode)
	}
	var document davMultiStatus
	decoder := xml.NewDecoder(io.LimitReader(response.Body, 32<<20))
	if err := decoder.Decode(&document); err != nil {
		return nil, "", false, fmt.Errorf("decode DAV sync-collection: %w", err)
	}
	if document.SyncToken == "" {
		return nil, "", false, errors.New("DAV sync-collection returned no sync token")
	}
	objects := make([]Object, 0, len(document.Responses))
	for _, item := range document.Responses {
		remoteID := davPath(item.Href)
		if remoteID == "" || samePath(remoteID, davPath(collection.RemoteID)) || strings.Contains(item.Status, " 404 ") {
			continue
		}
		for _, propstat := range item.PropStats {
			if !strings.Contains(propstat.Status, " 200 ") {
				continue
			}
			objects = append(objects, Object{RemoteID: remoteID, URL: c.resolve(item.Href), ETag: strings.Trim(propstat.Prop.ETag, `"`), ContentType: propstat.Prop.ContentType})
			break
		}
	}
	return objects, document.SyncToken, true, nil
}

type davMultiStatus struct {
	SyncToken string        `xml:"sync-token"`
	Responses []davResponse `xml:"response"`
}
type davResponse struct {
	Href      string        `xml:"href"`
	Status    string        `xml:"status"`
	PropStats []davPropStat `xml:"propstat"`
}
type davPropStat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}
type davProp struct {
	ETag        string `xml:"getetag"`
	ContentType string `xml:"getcontenttype"`
}

func davPath(value string) string {
	parsed, err := url.Parse(value)
	if err == nil && parsed.Path != "" {
		return parsed.Path
	}
	return value
}

func xmlEscape(value string) string {
	var output strings.Builder
	_ = xml.EscapeText(&output, []byte(value))
	return output.String()
}

func (c *Client) Get(ctx context.Context, object Object) (io.ReadCloser, string, error) {
	objectURL := object.URL
	if objectURL == "" {
		objectURL = c.resolve(object.RemoteID)
	}
	target, err := url.Parse(objectURL)
	if err != nil || !sameOrigin(c.endpoint, target) {
		return nil, "", errors.New("refusing to send DAV credentials to another origin")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", err
	}
	setAuth(request, c.account)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, "", fmt.Errorf("DAV download returned HTTP %d", response.StatusCode)
	}
	contentType := object.ContentType
	if contentType == "" {
		contentType = response.Header.Get("Content-Type")
	}
	if contentType == "" {
		if c.account.Protocol == "carddav" {
			contentType = "text/vcard"
		} else {
			contentType = "text/calendar"
		}
	}
	return response.Body, contentType, nil
}

// Put sends the archived bytes unchanged. The typed DAV helpers re-encode
// parsed objects, and webdav.Client.Create omits the media type.
func (c *Client) Put(ctx context.Context, collectionURL, kind, contentType string, body io.Reader) (string, error) {
	collectionPath := collectionURL
	if parsed, err := url.Parse(collectionURL); err == nil && parsed.Path != "" {
		collectionPath = parsed.Path
	}
	extension := ".vcf"
	if kind == "calendar" {
		extension = ".ics"
	}
	target := strings.TrimSuffix(collectionPath, "/") + "/pimbackup-" + uuid.New().String() + extension
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.resolve(target), io.NopCloser(body))
	if err != nil {
		return "", err
	}
	if contentType == "" {
		if kind == "calendar" {
			contentType = "text/calendar"
		} else {
			contentType = "text/vcard"
		}
	}
	if !sameOrigin(c.endpoint, request.URL) {
		return "", errors.New("refusing DAV restore to another origin")
	}
	request.Header.Set("Content-Type", contentType)
	if seeker, ok := body.(io.Seeker); ok {
		current, currentErr := seeker.Seek(0, io.SeekCurrent)
		end, endErr := seeker.Seek(0, io.SeekEnd)
		_, restoreErr := seeker.Seek(current, io.SeekStart)
		if currentErr == nil && endErr == nil && restoreErr == nil && end >= current {
			request.ContentLength = end - current
		}
	}
	setAuth(request, c.account)
	response, err := c.http.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("DAV restore returned HTTP %d", response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != "" {
		parsed, parseErr := url.Parse(location)
		if parseErr != nil {
			return "", fmt.Errorf("parse DAV restore location: %w", parseErr)
		}
		return response.Request.URL.ResolveReference(parsed).String(), nil
	}
	return c.resolve(target), nil
}

func (c *Client) resolve(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}
	return c.endpoint.ResolveReference(parsed).String()
}
func collectionName(name, value, fallback string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	if base := path.Base(strings.TrimSuffix(value, "/")); base != "." && base != "/" && base != "" {
		return base
	}
	return fallback
}
func samePath(left, right string) bool {
	return strings.TrimSuffix(left, "/") == strings.TrimSuffix(right, "/")
}

type authenticatedClient struct {
	client  *http.Client
	account config.AccountConfig
	origin  *url.URL
}

func (c authenticatedClient) Do(request *http.Request) (*http.Response, error) {
	if !sameOrigin(c.origin, request.URL) {
		return nil, errors.New("refusing to send DAV credentials to another origin")
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	setAuth(clone, c.account)
	return c.client.Do(clone)
}
func sameOrigin(first, second *url.URL) bool {
	return first != nil && second != nil && strings.EqualFold(first.Scheme, second.Scheme) && strings.EqualFold(first.Host, second.Host) && first.User == nil && second.User == nil
}

func setAuth(request *http.Request, account config.AccountConfig) {
	if account.Auth == "bearer" {
		request.Header.Set("Authorization", "Bearer "+account.ResolvedToken)
	} else {
		request.SetBasicAuth(account.Username, account.ResolvedPassword)
	}
}
