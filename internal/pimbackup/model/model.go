// Package model contains PIM Backup domain records shared by its catalog,
// storage, protocol, and CLI code.
package model

import "time"

type Account struct {
	ID          string    `json:"id"`
	Protocol    string    `json:"protocol"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Mailboxes   int       `json:"mailboxes"`
	Messages    int       `json:"messages"`
	Collections int       `json:"collections"`
	Objects     int       `json:"objects"`
}

type Mailbox struct {
	ID             int64      `json:"id"`
	AccountID      string     `json:"account_id"`
	Name           string     `json:"name"`
	PathKey        string     `json:"path_key"`
	Delimiter      string     `json:"delimiter,omitempty"`
	UIDValidity    uint32     `json:"uid_validity"`
	LastUID        uint32     `json:"last_uid"`
	RemoteMessages uint32     `json:"remote_messages"`
	Active         bool       `json:"active"`
	CreatedAt      time.Time  `json:"created_at"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	Messages       int        `json:"messages"`
}

type Message struct {
	ID              int64      `json:"id"`
	MailboxID       int64      `json:"mailbox_id"`
	AccountID       string     `json:"account_id"`
	Mailbox         string     `json:"mailbox"`
	UIDValidity     uint32     `json:"uid_validity"`
	UID             uint32     `json:"uid"`
	InternalDate    *time.Time `json:"internal_date,omitempty"`
	Size            int64      `json:"size"`
	SHA256          string     `json:"sha256"`
	Path            string     `json:"path"`
	SidecarPath     string     `json:"sidecar_path"`
	Flags           []string   `json:"flags"`
	Subject         string     `json:"subject,omitempty"`
	From            string     `json:"from,omitempty"`
	To              string     `json:"to,omitempty"`
	HeaderMessageID string     `json:"header_message_id,omitempty"`
	HeaderDate      *time.Time `json:"header_date,omitempty"`
	ParseError      string     `json:"parse_error,omitempty"`
	ArchivedAt      time.Time  `json:"archived_at"`
	LastVerifiedAt  *time.Time `json:"last_verified_at,omitempty"`
	VerifyError     string     `json:"verify_error,omitempty"`
}

type MessageFilter struct {
	AccountID   string
	Mailbox     string
	UIDValidity uint32
	Limit       int
	Offset      int
}

type Collection struct {
	ID         int64      `json:"id"`
	AccountID  string     `json:"account_id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	RemoteID   string     `json:"remote_id"`
	RemoteURL  string     `json:"remote_url,omitempty"`
	SyncToken  string     `json:"sync_token,omitempty"`
	Active     bool       `json:"active"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	Objects    int        `json:"objects"`
}

type Object struct {
	ID                 int64      `json:"id"`
	CollectionID       int64      `json:"collection_id"`
	AccountID          string     `json:"account_id"`
	Collection         string     `json:"collection"`
	CollectionRemoteID string     `json:"collection_remote_id"`
	Kind               string     `json:"kind"`
	RemoteID           string     `json:"remote_id"`
	ETag               string     `json:"etag,omitempty"`
	ContentType        string     `json:"content_type"`
	Size               int64      `json:"size"`
	SHA256             string     `json:"sha256"`
	Path               string     `json:"path"`
	SidecarPath        string     `json:"sidecar_path"`
	Title              string     `json:"title,omitempty"`
	Flags              []string   `json:"flags,omitempty"`
	InternalDate       *time.Time `json:"internal_date,omitempty"`
	RemoteCollections  []string   `json:"remote_collections,omitempty"`
	ArchivedAt         time.Time  `json:"archived_at"`
	LastVerifiedAt     *time.Time `json:"last_verified_at,omitempty"`
	VerifyError        string     `json:"verify_error,omitempty"`
}

type ObjectFilter struct {
	AccountID  string
	Collection string
	Kind       string
	Limit      int
	Offset     int
}

type BackupRequest struct {
	Accounts []string `json:"accounts,omitempty"`
}

type AccountBackupResult struct {
	AccountID   string                   `json:"account_id"`
	Mailboxes   []MailboxBackupResult    `json:"mailboxes,omitempty"`
	Collections []CollectionBackupResult `json:"collections,omitempty"`
	Fetched     int                      `json:"fetched"`
	Bytes       int64                    `json:"bytes"`
	Error       string                   `json:"error,omitempty"`
}

type CollectionBackupResult struct {
	Collection string `json:"collection"`
	Kind       string `json:"kind"`
	Found      int    `json:"found"`
	Fetched    int    `json:"fetched"`
	Bytes      int64  `json:"bytes"`
	Error      string `json:"error,omitempty"`
}

type MailboxBackupResult struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity,omitempty"`
	Found       int    `json:"found"`
	Fetched     int    `json:"fetched"`
	Bytes       int64  `json:"bytes"`
	Error       string `json:"error,omitempty"`
}

type BackupReport struct {
	Accounts []AccountBackupResult `json:"accounts"`
	Fetched  int                   `json:"fetched"`
	Bytes    int64                 `json:"bytes"`
	Errors   int                   `json:"errors"`
}

type VerifyRequest struct {
	AccountID string `json:"account_id,omitempty"`
	MessageID int64  `json:"message_id,omitempty"`
	ObjectID  int64  `json:"object_id,omitempty"`
}

type VerificationIssue struct {
	MessageID int64  `json:"message_id,omitempty"`
	ObjectID  int64  `json:"object_id,omitempty"`
	Path      string `json:"path,omitempty"`
	Error     string `json:"error"`
}

type VerifyReport struct {
	Checked         int                 `json:"checked"`
	Passed          int                 `json:"passed"`
	Failed          int                 `json:"failed"`
	Issues          []VerificationIssue `json:"issues,omitempty"`
	IssuesTruncated bool                `json:"issues_truncated,omitempty"`
}

type RestoreRequest struct {
	MessageIDs       []int64 `json:"message_ids,omitempty"`
	ObjectIDs        []int64 `json:"object_ids,omitempty"`
	TargetAccount    string  `json:"target_account"`
	TargetMailbox    string  `json:"target_mailbox,omitempty"`
	TargetCollection string  `json:"target_collection,omitempty"`
	CreateMailbox    bool    `json:"create_mailbox,omitempty"`
	Confirm          bool    `json:"confirm"`
}

type RestoredMessage struct {
	MessageID   int64  `json:"message_id"`
	RemoteID    string `json:"remote_id,omitempty"`
	UID         uint32 `json:"uid,omitempty"`
	UIDValidity uint32 `json:"uid_validity,omitempty"`
	Error       string `json:"error,omitempty"`
}

type RestoredObject struct {
	ObjectID    int64  `json:"object_id"`
	RemoteID    string `json:"remote_id,omitempty"`
	UID         uint32 `json:"uid,omitempty"`
	UIDValidity uint32 `json:"uid_validity,omitempty"`
	Error       string `json:"error,omitempty"`
}

type RestoreReport struct {
	TargetAccount    string            `json:"target_account"`
	TargetMailbox    string            `json:"target_mailbox,omitempty"`
	TargetCollection string            `json:"target_collection,omitempty"`
	Restored         int               `json:"restored"`
	Failed           int               `json:"failed"`
	Messages         []RestoredMessage `json:"messages,omitempty"`
	Objects          []RestoredObject  `json:"objects,omitempty"`
}

type CheckResult struct {
	Name     string        `json:"name"`
	Status   string        `json:"status"`
	Message  string        `json:"message,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

type CheckReport struct {
	Status string        `json:"status"`
	Checks []CheckResult `json:"checks"`
}

type RebuildReport struct {
	Mailboxes   int      `json:"mailboxes"`
	Messages    int      `json:"messages"`
	Collections int      `json:"collections"`
	Objects     int      `json:"objects"`
	Errors      []string `json:"errors,omitempty"`
}
