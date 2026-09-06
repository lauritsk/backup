// Package mailstore owns PIM Backup's canonical mail files and sidecars.
package mailstore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lauritsk/backup/internal/atomicfile"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

const (
	mailboxFormat = "pimbackup-mailbox/v1"
	messageFormat = "pimbackup-message/v1"
)

type Store struct {
	dataDir     string
	accountsDir string
	root        *os.Root
}

type MailboxMetadata struct {
	Format      string    `json:"format"`
	AccountID   string    `json:"account_id"`
	Mailbox     string    `json:"mailbox"`
	PathKey     string    `json:"path_key"`
	Delimiter   string    `json:"delimiter,omitempty"`
	UIDValidity uint32    `json:"uid_validity"`
	CreatedAt   time.Time `json:"created_at"`
}

type MessageMetadata struct {
	Format       string     `json:"format"`
	AccountID    string     `json:"account_id"`
	Mailbox      string     `json:"mailbox"`
	UIDValidity  uint32     `json:"uid_validity"`
	UID          uint32     `json:"uid"`
	InternalDate *time.Time `json:"internal_date,omitempty"`
	Flags        []string   `json:"flags"`
	Size         int64      `json:"size"`
	SHA256       string     `json:"sha256"`
	ArchivedAt   time.Time  `json:"archived_at"`
	Recovered    bool       `json:"recovered,omitempty"`
}

type FetchedMessage struct {
	UID          uint32
	InternalDate *time.Time
	Flags        []string
	ExpectedSize int64
	Body         io.Reader
}

type SavedMessage struct {
	Message model.Message
	Created bool
}

type ScannedMailbox struct {
	Metadata MailboxMetadata
	Messages []model.Message
}

type ScanResult struct {
	Mailboxes []ScannedMailbox
	Errors    []error
}

func New(dataDir string) (*Store, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, errors.New("mail store data directory must be absolute")
	}
	dataDir = filepath.Clean(dataDir)
	info, err := os.Lstat(dataDir)
	if err != nil {
		return nil, fmt.Errorf("inspect mail store data directory: %w", err)
	}
	if !info.Mode().IsDir() {
		return nil, errors.New("mail store data path is not a directory")
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("set mail store data directory permissions: %w", err)
	}
	accountsDir := filepath.Join(dataDir, "accounts")
	if err := ensurePrivateDirectories(dataDir, accountsDir); err != nil {
		return nil, fmt.Errorf("create accounts directory: %w", err)
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, fmt.Errorf("open mail store root: %w", err)
	}
	return &Store{dataDir: dataDir, accountsDir: accountsDir, root: root}, nil
}

func (s *Store) Close() error { return s.root.Close() }

func (s *Store) DataDir() string {
	return s.dataDir
}

func PathKey(mailbox string) string {
	var slug strings.Builder
	lastSeparator := false
	for _, r := range mailbox {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if slug.Len() < 48 {
				slug.WriteRune(r)
			}
			lastSeparator = false
			continue
		}
		if !lastSeparator && slug.Len() > 0 && slug.Len() < 48 {
			slug.WriteByte('-')
			lastSeparator = true
		}
	}
	readable := strings.Trim(slug.String(), "-")
	if readable == "" {
		readable = "mailbox"
	}
	digest := sha256.Sum256([]byte(mailbox))
	return readable + "--" + hex.EncodeToString(digest[:12])
}

func (s *Store) PrepareMailbox(mailbox model.Mailbox) (string, error) {
	if mailbox.AccountID == "" || mailbox.Name == "" || mailbox.UIDValidity == 0 {
		return "", errors.New("mailbox account, name, and UIDVALIDITY are required")
	}
	pathKey := mailbox.PathKey
	if pathKey == "" {
		pathKey = PathKey(mailbox.Name)
	}
	generationDir, err := s.generationDir(mailbox.AccountID, pathKey, mailbox.UIDValidity)
	if err != nil {
		return "", err
	}
	if err := ensurePrivateDirectories(s.accountsDir, generationDir); err != nil {
		return "", fmt.Errorf("create mailbox directory: %w", err)
	}

	metadataPath := filepath.Join(generationDir, "mailbox.json")
	if existing, err := readMailboxMetadata(metadataPath); err == nil {
		if existing.AccountID != mailbox.AccountID || existing.Mailbox != mailbox.Name || existing.UIDValidity != mailbox.UIDValidity || existing.PathKey != pathKey {
			return "", fmt.Errorf("mailbox metadata conflict at %s", metadataPath)
		}
		return generationDir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	metadata := MailboxMetadata{
		Format:      mailboxFormat,
		AccountID:   mailbox.AccountID,
		Mailbox:     mailbox.Name,
		PathKey:     pathKey,
		Delimiter:   mailbox.Delimiter,
		UIDValidity: mailbox.UIDValidity,
		CreatedAt:   time.Now().UTC(),
	}
	if err := writeJSON(metadataPath, metadata); err != nil {
		return "", fmt.Errorf("write mailbox metadata: %w", err)
	}
	return generationDir, nil
}

func (s *Store) Save(ctx context.Context, mailbox model.Mailbox, fetched FetchedMessage) (SavedMessage, error) {
	if fetched.UID == 0 {
		return SavedMessage{}, errors.New("message UID must be greater than zero")
	}
	if fetched.Body == nil {
		return SavedMessage{}, errors.New("message body is missing")
	}
	generationDir, err := s.PrepareMailbox(mailbox)
	if err != nil {
		return SavedMessage{}, err
	}

	base := strconv.FormatUint(uint64(fetched.UID), 10)
	messagePath := filepath.Join(generationDir, base+".eml")
	sidecarPath := filepath.Join(generationDir, base+".json")
	temp, err := os.CreateTemp(generationDir, "."+base+".eml.tmp-")
	if err != nil {
		return SavedMessage{}, fmt.Errorf("create message temporary file: %w", err)
	}
	tempPath := temp.Name()
	keepTemp := false
	defer func() {
		_ = temp.Close()
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return SavedMessage{}, fmt.Errorf("set message permissions: %w", err)
	}

	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, hash), &contextReader{ctx: ctx, reader: fetched.Body})
	if err != nil {
		return SavedMessage{}, fmt.Errorf("write message UID %d: %w", fetched.UID, err)
	}
	if fetched.ExpectedSize >= 0 && written != fetched.ExpectedSize {
		return SavedMessage{}, fmt.Errorf("message UID %d size is %d bytes, server reported %d", fetched.UID, written, fetched.ExpectedSize)
	}
	if err := temp.Sync(); err != nil {
		return SavedMessage{}, fmt.Errorf("sync message UID %d: %w", fetched.UID, err)
	}
	if err := temp.Close(); err != nil {
		return SavedMessage{}, fmt.Errorf("close message UID %d: %w", fetched.UID, err)
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	created := true
	if info, statErr := os.Lstat(messagePath); statErr == nil {
		if !info.Mode().IsRegular() {
			return SavedMessage{}, fmt.Errorf("message destination %s is not a regular file", messagePath)
		}
		existingDigest, existingSize, hashErr := hashFile(messagePath)
		if hashErr != nil {
			return SavedMessage{}, hashErr
		}
		if existingDigest != digest || existingSize != written {
			conflictPath, conflictErr := s.keepConflict(tempPath, messagePath)
			if conflictErr != nil {
				return SavedMessage{}, conflictErr
			}
			keepTemp = true
			return SavedMessage{}, fmt.Errorf("message identity conflict for UID %d; fetched content kept at %s", fetched.UID, conflictPath)
		}
		created = false
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return SavedMessage{}, fmt.Errorf("inspect message destination: %w", statErr)
	} else {
		if err := os.Rename(tempPath, messagePath); err != nil {
			return SavedMessage{}, fmt.Errorf("commit message UID %d: %w", fetched.UID, err)
		}
		keepTemp = true
		if err := atomicfile.SyncDir(generationDir); err != nil {
			return SavedMessage{}, err
		}
	}

	archivedAt := time.Now().UTC()
	if existing, readErr := readMessageMetadata(sidecarPath); readErr == nil {
		if existing.AccountID == mailbox.AccountID && existing.Mailbox == mailbox.Name && existing.UIDValidity == mailbox.UIDValidity && existing.UID == fetched.UID && existing.SHA256 == digest {
			archivedAt = existing.ArchivedAt
		}
	}
	metadata := MessageMetadata{
		Format:       messageFormat,
		AccountID:    mailbox.AccountID,
		Mailbox:      mailbox.Name,
		UIDValidity:  mailbox.UIDValidity,
		UID:          fetched.UID,
		InternalDate: copyTime(fetched.InternalDate),
		Flags:        append([]string(nil), fetched.Flags...),
		Size:         written,
		SHA256:       digest,
		ArchivedAt:   archivedAt,
	}
	if metadata.Flags == nil {
		metadata.Flags = []string{}
	}
	if err := writeJSON(sidecarPath, metadata); err != nil {
		return SavedMessage{}, fmt.Errorf("write message sidecar: %w", err)
	}

	relativeMessage, err := s.relative(messagePath)
	if err != nil {
		return SavedMessage{}, err
	}
	relativeSidecar, err := s.relative(sidecarPath)
	if err != nil {
		return SavedMessage{}, err
	}
	message := model.Message{
		MailboxID:    mailbox.ID,
		AccountID:    mailbox.AccountID,
		Mailbox:      mailbox.Name,
		UIDValidity:  mailbox.UIDValidity,
		UID:          fetched.UID,
		InternalDate: copyTime(fetched.InternalDate),
		Size:         written,
		SHA256:       digest,
		Path:         relativeMessage,
		SidecarPath:  relativeSidecar,
		Flags:        append([]string(nil), fetched.Flags...),
		ArchivedAt:   archivedAt,
	}
	populateHeaders(messagePath, &message)
	return SavedMessage{Message: message, Created: created}, nil
}

func (s *Store) keepConflict(tempPath, messagePath string) (string, error) {
	suffix, err := randomSuffix()
	if err != nil {
		return "", err
	}
	conflictPath := strings.TrimSuffix(messagePath, ".eml") + ".conflict-" + suffix + ".eml"
	if err := os.Rename(tempPath, conflictPath); err != nil {
		return "", fmt.Errorf("keep conflicting message: %w", err)
	}
	if err := atomicfile.SyncDir(filepath.Dir(messagePath)); err != nil {
		return "", err
	}
	return conflictPath, nil
}

func randomSuffix() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate conflict suffix: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Store) generationDir(accountID, pathKey string, uidValidity uint32) (string, error) {
	if strings.ContainsAny(accountID, `/\\`) || accountID == "." || accountID == ".." {
		return "", errors.New("unsafe account ID")
	}
	if strings.ContainsAny(pathKey, `/\\`) || pathKey == "." || pathKey == ".." {
		return "", errors.New("unsafe mailbox path key")
	}
	path := filepath.Join(s.accountsDir, accountID, "mail", pathKey, "uidvalidity-"+strconv.FormatUint(uint64(uidValidity), 10))
	if _, err := s.relative(path); err != nil {
		return "", err
	}
	if err := rejectSymlinks(s.dataDir, path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) relative(filename string) (string, error) {
	relative, err := filepath.Rel(s.dataDir, filename)
	if err != nil {
		return "", fmt.Errorf("make data path relative: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path %q escapes data directory", filename)
	}
	return filepath.ToSlash(relative), nil
}

func (s *Store) Resolve(relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("stored path must be a non-empty relative path")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("stored path escapes data directory")
	}
	filename := filepath.Join(s.dataDir, clean)
	if _, err := s.relative(filename); err != nil {
		return "", err
	}
	if err := rejectSymlinks(s.dataDir, filename); err != nil {
		return "", err
	}
	return filename, nil
}

func (s *Store) OpenMessage(message model.Message) (*os.File, error) {
	if _, err := s.Resolve(message.Path); err != nil {
		return nil, err
	}
	file, err := s.root.Open(filepath.ToSlash(message.Path))
	if err != nil {
		return nil, fmt.Errorf("open message file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("inspect message file: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("message path is not a regular file")
	}
	return file, nil
}

func (s *Store) BasicCheck(message model.Message) error {
	messagePath, err := s.Resolve(message.Path)
	if err != nil {
		return err
	}
	info, err := os.Lstat(messagePath)
	if err != nil {
		return fmt.Errorf("message payload: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("message payload is not a regular file")
	}
	if info.Size() != message.Size {
		return fmt.Errorf("message payload size is %d, catalog records %d", info.Size(), message.Size)
	}
	expectedBase := strconv.FormatUint(uint64(message.UID), 10)
	if filepath.Base(messagePath) != expectedBase+".eml" {
		return errors.New("message payload name does not match its UID")
	}
	sidecarPath, err := s.Resolve(message.SidecarPath)
	if err != nil {
		return err
	}
	if filepath.Dir(sidecarPath) != filepath.Dir(messagePath) || filepath.Base(sidecarPath) != expectedBase+".json" {
		return errors.New("message sidecar path does not match its payload")
	}
	metadata, err := readMessageMetadata(sidecarPath)
	if err != nil {
		return err
	}
	if metadata.AccountID != message.AccountID || metadata.Mailbox != message.Mailbox || metadata.UIDValidity != message.UIDValidity || metadata.UID != message.UID {
		return errors.New("message sidecar identity does not match the catalog")
	}
	if metadata.Size != info.Size() {
		return errors.New("message sidecar size does not match the payload")
	}
	mailboxMetadata, err := readMailboxMetadata(filepath.Join(filepath.Dir(messagePath), "mailbox.json"))
	if err != nil {
		return err
	}
	if mailboxMetadata.AccountID != message.AccountID || mailboxMetadata.Mailbox != message.Mailbox || mailboxMetadata.UIDValidity != message.UIDValidity {
		return errors.New("mailbox sidecar identity does not match the catalog")
	}
	generationDir := filepath.Dir(messagePath)
	mailDir := filepath.Dir(filepath.Dir(generationDir))
	accountDir := filepath.Dir(mailDir)
	if filepath.Base(filepath.Dir(generationDir)) != mailboxMetadata.PathKey ||
		filepath.Base(generationDir) != "uidvalidity-"+strconv.FormatUint(uint64(message.UIDValidity), 10) ||
		filepath.Base(mailDir) != "mail" || filepath.Base(accountDir) != message.AccountID {
		return errors.New("mailbox directory does not match its sidecar")
	}
	return nil
}

func (s *Store) VerifyIntegrity(ctx context.Context, message model.Message) error {
	if err := s.BasicCheck(message); err != nil {
		return err
	}
	messagePath, _ := s.Resolve(message.Path)
	digest, size, err := hashFileContext(ctx, messagePath)
	if err != nil {
		return err
	}
	var problems []error
	if size != message.Size {
		problems = append(problems, fmt.Errorf("size is %d, catalog records %d", size, message.Size))
	}
	if digest != message.SHA256 {
		problems = append(problems, fmt.Errorf("SHA-256 is %s, catalog records %s", digest, message.SHA256))
	}

	sidecarPath, _ := s.Resolve(message.SidecarPath)
	metadata, err := readMessageMetadata(sidecarPath)
	if err != nil {
		problems = append(problems, err)
	} else {
		if metadata.Size != size {
			problems = append(problems, fmt.Errorf("sidecar size is %d, file is %d", metadata.Size, size))
		}
		if metadata.SHA256 != digest {
			problems = append(problems, fmt.Errorf("sidecar SHA-256 is %s, file is %s", metadata.SHA256, digest))
		}
		if !slices.Equal(metadata.Flags, message.Flags) {
			problems = append(problems, errors.New("sidecar flags do not match the catalog"))
		}
		if !equalTimePointers(metadata.InternalDate, message.InternalDate) {
			problems = append(problems, errors.New("sidecar internal date does not match the catalog"))
		}
		if !metadata.ArchivedAt.Equal(message.ArchivedAt) {
			problems = append(problems, errors.New("sidecar archive time does not match the catalog"))
		}
	}
	return errors.Join(problems...)
}

func (s *Store) Verify(ctx context.Context, message model.Message) error {
	integrityErr := s.VerifyIntegrity(ctx, message)
	messagePath, pathErr := s.Resolve(message.Path)
	if pathErr != nil {
		return errors.Join(integrityErr, pathErr)
	}
	return errors.Join(integrityErr, verifyMessageSyntax(ctx, messagePath))
}

// RecoverMissingSidecars recreates sidecars lost between the durable payload
// rename and the sidecar commit. Catalog metadata wins when it is available.
// Otherwise the payload path supplies the IMAP identity and the method records
// that the remaining metadata was recovered.
func (s *Store) RecoverMissingSidecars(ctx context.Context, known []model.Message) (recovered []string, problems []error) {
	knownPayloads := make(map[string]struct{}, len(known))
	for _, message := range known {
		messagePath, err := s.Resolve(message.Path)
		if err != nil {
			problems = append(problems, fmt.Errorf("recover sidecar for message %d: %w", message.ID, err))
			continue
		}
		knownPayloads[messagePath] = struct{}{}
		sidecarPath, err := s.Resolve(message.SidecarPath)
		if err != nil {
			problems = append(problems, fmt.Errorf("recover sidecar for message %d: %w", message.ID, err))
			continue
		}
		if _, err := os.Lstat(sidecarPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("inspect sidecar for message %d: %w", message.ID, err))
			continue
		}
		if filepath.Dir(sidecarPath) != filepath.Dir(messagePath) ||
			filepath.Base(messagePath) != strconv.FormatUint(uint64(message.UID), 10)+".eml" ||
			filepath.Base(sidecarPath) != strconv.FormatUint(uint64(message.UID), 10)+".json" {
			problems = append(problems, fmt.Errorf("recover sidecar for message %d: payload and sidecar paths do not match the UID", message.ID))
			continue
		}
		digest, size, err := hashFileContext(ctx, messagePath)
		if err != nil {
			problems = append(problems, fmt.Errorf("recover sidecar for message %d: %w", message.ID, err))
			continue
		}
		if size != message.Size || digest != message.SHA256 {
			problems = append(problems, fmt.Errorf("recover sidecar for message %d: payload does not match the catalog", message.ID))
			continue
		}
		metadata := MessageMetadata{
			Format:       messageFormat,
			AccountID:    message.AccountID,
			Mailbox:      message.Mailbox,
			UIDValidity:  message.UIDValidity,
			UID:          message.UID,
			InternalDate: copyTime(message.InternalDate),
			Flags:        append([]string(nil), message.Flags...),
			Size:         size,
			SHA256:       digest,
			ArchivedAt:   message.ArchivedAt,
		}
		if metadata.Flags == nil {
			metadata.Flags = []string{}
		}
		if err := writeJSON(sidecarPath, metadata); err != nil {
			problems = append(problems, fmt.Errorf("recover sidecar for message %d: %w", message.ID, err))
			continue
		}
		relative, _ := s.relative(sidecarPath)
		recovered = append(recovered, relative)
	}

	walkErr := filepath.WalkDir(s.accountsDir, func(metadataPath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			problems = append(problems, walkErr)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "mailbox.json" {
			return nil
		}
		mailbox, err := readMailboxMetadata(metadataPath)
		if err != nil || !mailboxMetadataMatchesDirectory(metadataPath, mailbox) {
			return nil
		}
		generationDir := filepath.Dir(metadataPath)
		entries, err := os.ReadDir(generationDir)
		if err != nil {
			problems = append(problems, fmt.Errorf("read %s while recovering sidecars: %w", generationDir, err))
			return nil
		}
		for _, child := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if child.IsDir() || !strings.HasSuffix(child.Name(), ".eml") {
				continue
			}
			uidValue := strings.TrimSuffix(child.Name(), ".eml")
			uid, err := strconv.ParseUint(uidValue, 10, 32)
			if err != nil || uid == 0 {
				continue
			}
			messagePath := filepath.Join(generationDir, child.Name())
			if _, exists := knownPayloads[messagePath]; exists {
				continue
			}
			sidecarPath := filepath.Join(generationDir, uidValue+".json")
			if _, err := os.Lstat(sidecarPath); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				problems = append(problems, fmt.Errorf("inspect missing sidecar %s: %w", sidecarPath, err))
				continue
			}
			info, err := os.Lstat(messagePath)
			if err != nil || !info.Mode().IsRegular() {
				if err == nil {
					err = errors.New("payload is not a regular file")
				}
				problems = append(problems, fmt.Errorf("recover sidecar %s: %w", sidecarPath, err))
				continue
			}
			digest, size, err := hashFileContext(ctx, messagePath)
			if err != nil {
				problems = append(problems, fmt.Errorf("recover sidecar %s: %w", sidecarPath, err))
				continue
			}
			archivedAt := info.ModTime().UTC()
			metadata := MessageMetadata{
				Format:      messageFormat,
				AccountID:   mailbox.AccountID,
				Mailbox:     mailbox.Mailbox,
				UIDValidity: mailbox.UIDValidity,
				UID:         uint32(uid),
				Flags:       []string{},
				Size:        size,
				SHA256:      digest,
				ArchivedAt:  archivedAt,
				Recovered:   true,
			}
			if err := writeJSON(sidecarPath, metadata); err != nil {
				problems = append(problems, fmt.Errorf("recover sidecar %s: %w", sidecarPath, err))
				continue
			}
			relative, _ := s.relative(sidecarPath)
			recovered = append(recovered, relative)
		}
		return nil
	})
	if walkErr != nil {
		problems = append(problems, walkErr)
	}
	sort.Strings(recovered)
	return recovered, problems
}

func (s *Store) CleanupTemps(ctx context.Context) ([]string, error) {
	var removed []string
	err := filepath.WalkDir(s.accountsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".") || !strings.Contains(entry.Name(), ".tmp-") {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		relative, _ := s.relative(path)
		removed = append(removed, relative)
		return atomicfile.SyncDir(filepath.Dir(path))
	})
	if err != nil {
		return removed, fmt.Errorf("clean temporary mail files: %w", err)
	}
	sort.Strings(removed)
	return removed, nil
}

func (s *Store) Scan(ctx context.Context) ScanResult {
	var result ScanResult
	err := filepath.WalkDir(s.accountsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Errors = append(result.Errors, walkErr)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "mailbox.json" {
			return nil
		}
		mailboxMetadata, err := readMailboxMetadata(path)
		if err != nil {
			result.Errors = append(result.Errors, err)
			return nil
		}
		generationDir := filepath.Dir(path)
		if !mailboxMetadataMatchesDirectory(path, mailboxMetadata) {
			result.Errors = append(result.Errors, fmt.Errorf("mailbox metadata %s does not match its directory", path))
			return nil
		}
		scanned := ScannedMailbox{Metadata: mailboxMetadata}
		entries, err := os.ReadDir(generationDir)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("read %s: %w", generationDir, err))
			return nil
		}
		sidecarUIDs := make(map[uint32]struct{})
		for _, child := range entries {
			if child.IsDir() || child.Name() == "mailbox.json" || !strings.HasSuffix(child.Name(), ".json") {
				continue
			}
			metadataPath := filepath.Join(generationDir, child.Name())
			metadata, err := readMessageMetadata(metadataPath)
			if err != nil {
				result.Errors = append(result.Errors, err)
				continue
			}
			if metadata.AccountID != mailboxMetadata.AccountID || metadata.Mailbox != mailboxMetadata.Mailbox || metadata.UIDValidity != mailboxMetadata.UIDValidity {
				result.Errors = append(result.Errors, fmt.Errorf("message sidecar %s does not match mailbox metadata", metadataPath))
				continue
			}
			expectedSidecarName := strconv.FormatUint(uint64(metadata.UID), 10) + ".json"
			if child.Name() != expectedSidecarName {
				result.Errors = append(result.Errors, fmt.Errorf("message sidecar %s has UID %d but should be named %s", metadataPath, metadata.UID, expectedSidecarName))
				continue
			}
			messagePath := filepath.Join(generationDir, strconv.FormatUint(uint64(metadata.UID), 10)+".eml")
			info, err := os.Lstat(messagePath)
			if err != nil || !info.Mode().IsRegular() {
				if err == nil {
					err = errors.New("not a regular file")
				}
				result.Errors = append(result.Errors, fmt.Errorf("message payload %s: %w", messagePath, err))
				continue
			}
			if info.Size() != metadata.Size {
				result.Errors = append(result.Errors, fmt.Errorf("message payload %s is %d bytes, sidecar records %d", messagePath, info.Size(), metadata.Size))
				continue
			}
			relativeMessage, _ := s.relative(messagePath)
			relativeSidecar, _ := s.relative(metadataPath)
			message := model.Message{
				AccountID:    metadata.AccountID,
				Mailbox:      metadata.Mailbox,
				UIDValidity:  metadata.UIDValidity,
				UID:          metadata.UID,
				InternalDate: copyTime(metadata.InternalDate),
				Size:         metadata.Size,
				SHA256:       metadata.SHA256,
				Path:         relativeMessage,
				SidecarPath:  relativeSidecar,
				Flags:        append([]string(nil), metadata.Flags...),
				ArchivedAt:   metadata.ArchivedAt,
			}
			populateHeaders(messagePath, &message)
			scanned.Messages = append(scanned.Messages, message)
			sidecarUIDs[metadata.UID] = struct{}{}
		}
		for _, child := range entries {
			if child.IsDir() || !strings.HasSuffix(child.Name(), ".eml") || strings.Contains(child.Name(), ".tmp-") {
				continue
			}
			base := strings.TrimSuffix(child.Name(), ".eml")
			uid, err := strconv.ParseUint(base, 10, 32)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("untracked message payload %s", filepath.Join(generationDir, child.Name())))
				continue
			}
			if _, ok := sidecarUIDs[uint32(uid)]; !ok {
				result.Errors = append(result.Errors, fmt.Errorf("message payload %s has no valid sidecar", filepath.Join(generationDir, child.Name())))
			}
		}
		sort.Slice(scanned.Messages, func(i, j int) bool { return scanned.Messages[i].UID < scanned.Messages[j].UID })
		result.Mailboxes = append(result.Mailboxes, scanned)
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		result.Errors = append(result.Errors, err)
	} else if err != nil {
		result.Errors = append(result.Errors, err)
	}
	sort.Slice(result.Mailboxes, func(i, j int) bool {
		left, right := result.Mailboxes[i].Metadata, result.Mailboxes[j].Metadata
		if left.AccountID != right.AccountID {
			return left.AccountID < right.AccountID
		}
		if left.Mailbox != right.Mailbox {
			return left.Mailbox < right.Mailbox
		}
		return left.UIDValidity < right.UIDValidity
	})
	return result
}

func mailboxMetadataMatchesDirectory(metadataPath string, metadata MailboxMetadata) bool {
	generationDir := filepath.Dir(metadataPath)
	mailDir := filepath.Dir(filepath.Dir(generationDir))
	accountDir := filepath.Dir(mailDir)
	return filepath.Base(filepath.Dir(generationDir)) == metadata.PathKey &&
		filepath.Base(generationDir) == "uidvalidity-"+strconv.FormatUint(uint64(metadata.UIDValidity), 10) &&
		filepath.Base(mailDir) == "mail" && filepath.Base(accountDir) == metadata.AccountID
}

func readMailboxMetadata(filename string) (MailboxMetadata, error) {
	var metadata MailboxMetadata
	if err := readJSON(filename, &metadata); err != nil {
		return MailboxMetadata{}, err
	}
	if metadata.Format != mailboxFormat {
		return MailboxMetadata{}, fmt.Errorf("mailbox metadata %s has unsupported format %q", filename, metadata.Format)
	}
	if metadata.AccountID == "" || metadata.Mailbox == "" || metadata.PathKey == "" || metadata.UIDValidity == 0 {
		return MailboxMetadata{}, fmt.Errorf("mailbox metadata %s is incomplete", filename)
	}
	return metadata, nil
}

func readMessageMetadata(filename string) (MessageMetadata, error) {
	var metadata MessageMetadata
	if err := readJSON(filename, &metadata); err != nil {
		return MessageMetadata{}, err
	}
	if metadata.Format != messageFormat {
		return MessageMetadata{}, fmt.Errorf("message metadata %s has unsupported format %q", filename, metadata.Format)
	}
	if metadata.AccountID == "" || metadata.Mailbox == "" || metadata.UIDValidity == 0 || metadata.UID == 0 || metadata.Size < 0 || metadata.ArchivedAt.IsZero() {
		return MessageMetadata{}, fmt.Errorf("message metadata %s is incomplete", filename)
	}
	decodedHash, err := hex.DecodeString(metadata.SHA256)
	if err != nil || len(decodedHash) != sha256.Size {
		return MessageMetadata{}, fmt.Errorf("message metadata %s has an invalid SHA-256", filename)
	}
	return metadata, nil
}

func readJSON(filename string, target any) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return fmt.Errorf("inspect metadata %s: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("metadata %s is not a regular file", filename)
	}
	if info.Size() > 1<<20 {
		return fmt.Errorf("metadata %s exceeds 1 MiB", filename)
	}
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open metadata %s: %w", filename, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode metadata %s: %w", filename, err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("decode metadata %s: trailing data", filename)
	}
	return nil
}

func writeJSON(filename string, value any) error {
	return atomicfile.Write(filename, 0o600, func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	})
}

func hashFile(filename string) (string, int64, error) {
	return hashFileContext(context.Background(), filename)
}

func hashFileContext(ctx context.Context, filename string) (string, int64, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", 0, fmt.Errorf("open %s for hashing: %w", filename, err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file})
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", filename, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func populateHeaders(filename string, message *model.Message) {
	file, err := os.Open(filename)
	if err != nil {
		message.ParseError = err.Error()
		return
	}
	defer file.Close()
	parsed, err := readMessageLimited(file)
	if err != nil {
		message.ParseError = err.Error()
		return
	}
	decoder := new(mime.WordDecoder)
	message.Subject, err = decoder.DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		message.Subject = parsed.Header.Get("Subject")
		message.ParseError = "decode Subject: " + err.Error()
	}
	message.From = parsed.Header.Get("From")
	message.To = parsed.Header.Get("To")
	message.HeaderMessageID = parsed.Header.Get("Message-ID")
	if value := parsed.Header.Get("Date"); value != "" {
		date, dateErr := netmail.ParseDate(value)
		if dateErr != nil {
			if message.ParseError != "" {
				message.ParseError += "; "
			}
			message.ParseError += "parse Date: " + dateErr.Error()
		} else {
			date = date.UTC()
			message.HeaderDate = &date
		}
	}
}

func verifyMessageSyntax(ctx context.Context, filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open message for MIME verification: %w", err)
	}
	defer file.Close()
	message, err := readMessageLimited(&contextReader{ctx: ctx, reader: file})
	if err != nil {
		return fmt.Errorf("parse RFC822 message: %w", err)
	}
	if err := verifyEntity(ctx, message.Header, message.Body, 0); err != nil {
		return fmt.Errorf("parse MIME message: %w", err)
	}
	return nil
}

func verifyEntity(ctx context.Context, header netmail.Header, body io.Reader, depth int) error {
	if depth > 64 {
		return errors.New("MIME nesting exceeds 64 levels")
	}
	decodedBody, err := transferDecodedReader(header.Get("Content-Transfer-Encoding"), body)
	if err != nil {
		return err
	}
	body = decodedBody
	contentType := header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(strings.ToLower(mediaType), "multipart/"):
		boundary := parameters["boundary"]
		if boundary == "" {
			return errors.New("multipart body has no boundary")
		}
		reader := multipart.NewReader(&contextReader{ctx: ctx, reader: body}, boundary)
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := verifyEntity(ctx, netmail.Header(part.Header), part, depth+1); err != nil {
				part.Close()
				return err
			}
			if err := part.Close(); err != nil {
				return err
			}
		}
	case strings.EqualFold(mediaType, "message/rfc822"):
		nested, err := readMessageLimited(&contextReader{ctx: ctx, reader: body})
		if err != nil {
			return err
		}
		return verifyEntity(ctx, nested.Header, nested.Body, depth+1)
	default:
		_, err := io.Copy(io.Discard, &contextReader{ctx: ctx, reader: body})
		return err
	}
}

func readMessageLimited(reader io.Reader) (*netmail.Message, error) {
	const maxHeaderBytes = 1 << 20
	buffered := bufio.NewReaderSize(reader, 32<<10)
	var header bytes.Buffer
	for {
		fragment, err := buffered.ReadSlice('\n')
		if header.Len()+len(fragment) > maxHeaderBytes {
			return nil, fmt.Errorf("message header exceeds %d bytes", maxHeaderBytes)
		}
		_, _ = header.Write(fragment)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if bytes.Equal(fragment, []byte("\n")) || bytes.Equal(fragment, []byte("\r\n")) || errors.Is(err, io.EOF) {
			break
		}
	}
	return netmail.ReadMessage(io.MultiReader(bytes.NewReader(header.Bytes()), buffered))
}

func transferDecodedReader(encoding string, body io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return body, nil
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body), nil
	case "quoted-printable":
		return quotedprintable.NewReader(body), nil
	default:
		return nil, fmt.Errorf("unsupported Content-Transfer-Encoding %q", encoding)
	}
}

func equalTimePointers(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func ensurePrivateDirectories(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("path %q escapes directory %s", target, root)
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, inspectErr := os.Lstat(current)
		if errors.Is(inspectErr, os.ErrNotExist) {
			mkdirErr := os.Mkdir(current, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return mkdirErr
			}
			info, inspectErr = os.Lstat(current)
		}
		if inspectErr != nil {
			return inspectErr
		}
		if !info.Mode().IsDir() {
			return fmt.Errorf("data path %s is not a directory", current)
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func rejectSymlinks(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("path %q escapes data directory", target)
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect data path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("data path %s is a symlink", current)
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
