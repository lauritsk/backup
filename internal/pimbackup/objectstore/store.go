// Package objectstore stores JMAP mail, vCard contacts, and iCalendar data.
package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-vcard"

	"github.com/lauritsk/backup/internal/atomicfile"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

const format = "pimbackup-object-v1"

type Store struct {
	dataDir     string
	accountsDir string
}

type CollectionMetadata struct {
	Format    string `json:"format"`
	AccountID string `json:"account_id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	RemoteID  string `json:"remote_id"`
	RemoteURL string `json:"remote_url,omitempty"`
	SyncToken string `json:"sync_token,omitempty"`
}

type Metadata struct {
	Format            string     `json:"format"`
	AccountID         string     `json:"account_id"`
	Kind              string     `json:"kind"`
	Collection        string     `json:"collection"`
	CollectionID      string     `json:"collection_id"`
	RemoteID          string     `json:"remote_id"`
	ETag              string     `json:"etag,omitempty"`
	ContentType       string     `json:"content_type"`
	Size              int64      `json:"size"`
	SHA256            string     `json:"sha256"`
	Title             string     `json:"title,omitempty"`
	Flags             []string   `json:"flags,omitempty"`
	InternalDate      *time.Time `json:"internal_date,omitempty"`
	RemoteCollections []string   `json:"remote_collections,omitempty"`
	ArchivedAt        time.Time  `json:"archived_at"`
}

type Attributes struct {
	Flags             []string
	InternalDate      *time.Time
	RemoteCollections []string
}

type Saved struct {
	Object  model.Object
	Created bool
}

type ScannedCollection struct {
	Metadata CollectionMetadata
	Objects  []model.Object
}

type ScanResult struct {
	Collections []ScannedCollection
	Errors      []error
}

func New(dataDir string) (*Store, error) {
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	accounts := filepath.Join(absolute, "accounts")
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		return nil, fmt.Errorf("create accounts directory: %w", err)
	}
	return &Store{dataDir: absolute, accountsDir: accounts}, nil
}

func (s *Store) PrepareCollection(collection model.Collection) (string, error) {
	dir, err := s.collectionDir(collection.AccountID, collection.Kind, collection.RemoteID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create collection directory: %w", err)
	}
	metadata := CollectionMetadata{Format: format, AccountID: collection.AccountID, Kind: collection.Kind,
		Name: collection.Name, RemoteID: collection.RemoteID, RemoteURL: collection.RemoteURL, SyncToken: collection.SyncToken}
	if err := writeJSON(filepath.Join(dir, "collection.json"), metadata); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *Store) Save(ctx context.Context, collection model.Collection, remoteID, etag, contentType string, attributes Attributes, body io.Reader) (Saved, error) {
	if strings.TrimSpace(remoteID) == "" {
		return Saved{}, errors.New("remote object ID cannot be empty")
	}
	dir, err := s.PrepareCollection(collection)
	if err != nil {
		return Saved{}, err
	}
	ext, expectedType, err := kindFormat(collection.Kind)
	if err != nil {
		return Saved{}, err
	}
	if contentType == "" {
		contentType = expectedType
	}
	name := objectKey(remoteID) + ext
	payloadPath := filepath.Join(dir, name)
	sidecarPath := strings.TrimSuffix(payloadPath, ext) + ".json"

	temp, err := os.CreateTemp(dir, ".object.tmp-")
	if err != nil {
		return Saved{}, fmt.Errorf("create object temporary file: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return Saved{}, err
	}
	hash := sha256.New()
	written, err := copyContext(ctx, io.MultiWriter(temp, hash), body)
	if err != nil {
		return Saved{}, fmt.Errorf("write object: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return Saved{}, fmt.Errorf("sync object: %w", err)
	}
	if err := temp.Close(); err != nil {
		return Saved{}, fmt.Errorf("close object: %w", err)
	}
	if _, err := verifyFile(tempPath, collection.Kind); err != nil {
		return Saved{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))

	created := true
	archivedAt := time.Now().UTC()
	if current, metadataErr := readMetadata(sidecarPath); metadataErr == nil {
		if current.RemoteID != remoteID || current.AccountID != collection.AccountID || current.Kind != collection.Kind || current.CollectionID != collection.RemoteID {
			return Saved{}, errors.New("existing object metadata has a conflicting identity")
		}
		created = false
		archivedAt = current.ArchivedAt
	} else if !errors.Is(metadataErr, os.ErrNotExist) {
		// A fresh, validated remote copy can repair a damaged sidecar.
		created = false
	}
	if err := os.Rename(tempPath, payloadPath); err != nil {
		return Saved{}, fmt.Errorf("commit object: %w", err)
	}
	committed = true
	if err := atomicfile.SyncDir(dir); err != nil {
		return Saved{}, err
	}

	title, _ := verifyFile(payloadPath, collection.Kind)
	metadata := Metadata{Format: format, AccountID: collection.AccountID, Kind: collection.Kind,
		Collection: collection.Name, CollectionID: collection.RemoteID, RemoteID: remoteID, ETag: etag, ContentType: contentType,
		Size: written, SHA256: digest, Title: title, Flags: append([]string(nil), attributes.Flags...), InternalDate: copyTime(attributes.InternalDate), RemoteCollections: append([]string(nil), attributes.RemoteCollections...), ArchivedAt: archivedAt}
	if err := writeJSON(sidecarPath, metadata); err != nil {
		return Saved{}, err
	}
	relPayload, err := s.relative(payloadPath)
	if err != nil {
		return Saved{}, err
	}
	relSidecar, err := s.relative(sidecarPath)
	if err != nil {
		return Saved{}, err
	}
	return Saved{Created: created, Object: model.Object{CollectionID: collection.ID, AccountID: collection.AccountID,
		Collection: collection.Name, CollectionRemoteID: collection.RemoteID, Kind: collection.Kind, RemoteID: remoteID, ETag: etag,
		ContentType: contentType, Size: written, SHA256: digest, Path: relPayload,
		SidecarPath: relSidecar, Title: title, Flags: append([]string(nil), attributes.Flags...), InternalDate: copyTime(attributes.InternalDate), RemoteCollections: append([]string(nil), attributes.RemoteCollections...), ArchivedAt: archivedAt}}, nil
}

func (s *Store) Open(object model.Object) (*os.File, error) {
	path, err := s.Resolve(object.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("object payload is not a regular file")
	}
	return os.Open(path)
}

func (s *Store) BasicCheck(object model.Object) error {
	path, err := s.Resolve(object.Path)
	if err != nil {
		return err
	}
	sidecar, err := s.Resolve(object.SidecarPath)
	if err != nil {
		return err
	}
	metadata, err := readMetadata(sidecar)
	if err != nil {
		return fmt.Errorf("read object sidecar: %w", err)
	}
	extension, _, err := kindFormat(object.Kind)
	if err != nil {
		return err
	}
	if filepath.Dir(path) != filepath.Dir(sidecar) || strings.TrimSuffix(filepath.Base(path), extension) != strings.TrimSuffix(filepath.Base(sidecar), ".json") {
		return errors.New("object payload and sidecar paths do not match")
	}
	if metadata.AccountID != object.AccountID || metadata.Kind != object.Kind || metadata.Collection != object.Collection || metadata.CollectionID != object.CollectionRemoteID || metadata.RemoteID != object.RemoteID || metadata.ETag != object.ETag || metadata.ContentType != object.ContentType || metadata.Size != object.Size || metadata.SHA256 != object.SHA256 || !slices.Equal(metadata.Flags, object.Flags) || !equalTime(metadata.InternalDate, object.InternalDate) || !slices.Equal(metadata.RemoteCollections, object.RemoteCollections) {
		return errors.New("object sidecar does not match the catalog")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("object payload is not a regular file")
	}
	if info.Size() != object.Size {
		return fmt.Errorf("size is %d, catalog records %d", info.Size(), object.Size)
	}
	return nil
}

func (s *Store) Verify(ctx context.Context, object model.Object) error {
	if err := s.BasicCheck(object); err != nil {
		return err
	}
	path, _ := s.Resolve(object.Path)
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := copyContext(ctx, hash, file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if size != object.Size {
		return fmt.Errorf("size changed while reading: got %d, expected %d", size, object.Size)
	}
	if digest := hex.EncodeToString(hash.Sum(nil)); digest != object.SHA256 {
		return fmt.Errorf("SHA-256 is %s, catalog records %s", digest, object.SHA256)
	}
	_, err = verifyFile(path, object.Kind)
	return err
}

func (s *Store) Resolve(relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("object path must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("object path escapes the data directory")
	}
	absolute := filepath.Join(s.dataDir, clean)
	rel, err := filepath.Rel(s.dataDir, absolute)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("object path escapes the data directory")
	}
	return absolute, nil
}

func (s *Store) Scan(ctx context.Context) ScanResult {
	result := ScanResult{}
	_ = filepath.WalkDir(s.accountsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Errors = append(result.Errors, walkErr)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "collection.json" {
			return nil
		}
		var collection CollectionMetadata
		if err := readJSON(path, &collection); err != nil {
			result.Errors = append(result.Errors, err)
			return nil
		}
		if collection.Format != format {
			result.Errors = append(result.Errors, fmt.Errorf("unsupported collection metadata %s", path))
			return nil
		}
		scanned := ScannedCollection{Metadata: collection}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			result.Errors = append(result.Errors, err)
			return nil
		}
		for _, child := range entries {
			if child.IsDir() {
				continue
			}
			if extension, _, formatErr := kindFormat(collection.Kind); formatErr == nil && strings.HasSuffix(child.Name(), extension) {
				sidecarName := strings.TrimSuffix(child.Name(), extension) + ".json"
				if _, err := os.Lstat(filepath.Join(filepath.Dir(path), sidecarName)); errors.Is(err, os.ErrNotExist) {
					result.Errors = append(result.Errors, fmt.Errorf("object payload %s has no sidecar", filepath.Join(filepath.Dir(path), child.Name())))
				}
			}
			if !strings.HasSuffix(child.Name(), ".json") || child.Name() == "collection.json" {
				continue
			}
			sidecar := filepath.Join(filepath.Dir(path), child.Name())
			metadata, err := readMetadata(sidecar)
			if err != nil {
				result.Errors = append(result.Errors, err)
				continue
			}
			if metadata.AccountID != collection.AccountID || metadata.Kind != collection.Kind || metadata.Collection != collection.Name || metadata.CollectionID != collection.RemoteID || metadata.RemoteID == "" || metadata.Size < 0 || len(metadata.SHA256) != 64 {
				result.Errors = append(result.Errors, fmt.Errorf("object metadata %s does not match its collection", sidecar))
				continue
			}
			ext, _, formatErr := kindFormat(metadata.Kind)
			if formatErr != nil {
				result.Errors = append(result.Errors, formatErr)
				continue
			}
			payload := strings.TrimSuffix(sidecar, ".json") + ext
			info, inspectErr := os.Lstat(payload)
			if inspectErr != nil || !info.Mode().IsRegular() {
				if inspectErr == nil {
					inspectErr = errors.New("payload is not a regular file")
				}
				result.Errors = append(result.Errors, fmt.Errorf("inspect object payload %s: %w", payload, inspectErr))
				continue
			}
			relPayload, err := s.relative(payload)
			if err != nil {
				result.Errors = append(result.Errors, err)
				continue
			}
			relSidecar, _ := s.relative(sidecar)
			scanned.Objects = append(scanned.Objects, model.Object{AccountID: metadata.AccountID, Collection: metadata.Collection, CollectionRemoteID: metadata.CollectionID,
				Kind: metadata.Kind, RemoteID: metadata.RemoteID, ETag: metadata.ETag, ContentType: metadata.ContentType,
				Size: metadata.Size, SHA256: metadata.SHA256, Path: relPayload, SidecarPath: relSidecar,
				Title: metadata.Title, Flags: append([]string(nil), metadata.Flags...), InternalDate: copyTime(metadata.InternalDate), RemoteCollections: append([]string(nil), metadata.RemoteCollections...), ArchivedAt: metadata.ArchivedAt})
		}
		sort.Slice(scanned.Objects, func(i, j int) bool { return scanned.Objects[i].RemoteID < scanned.Objects[j].RemoteID })
		result.Collections = append(result.Collections, scanned)
		return nil
	})
	return result
}

func (s *Store) collectionDir(accountID, kind, remoteID string) (string, error) {
	if !safeID(accountID) {
		return "", errors.New("unsafe account ID")
	}
	directory := ""
	switch kind {
	case "mail":
		directory = "mail-jmap"
	case "contact":
		directory = "contacts"
	case "calendar":
		directory = "calendars"
	default:
		return "", fmt.Errorf("unsupported object kind %q", kind)
	}
	return filepath.Join(s.accountsDir, accountID, directory, objectKey(remoteID)), nil
}

func (s *Store) relative(path string) (string, error) {
	rel, err := filepath.Rel(s.dataDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes data directory")
	}
	return filepath.ToSlash(rel), nil
}

func kindFormat(kind string) (extension, contentType string, err error) {
	switch kind {
	case "mail":
		return ".eml", "message/rfc822", nil
	case "contact":
		return ".vcf", "text/vcard", nil
	case "calendar":
		return ".ics", "text/calendar", nil
	default:
		return "", "", fmt.Errorf("unsupported object kind %q", kind)
	}
}

func verifyFile(path, kind string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if kind == "mail" {
		message, err := mail.ReadMessage(file)
		if err != nil {
			return "", fmt.Errorf("parse MIME message: %w", err)
		}
		return message.Header.Get("Subject"), nil
	}
	if kind == "contact" {
		card, err := vcard.NewDecoder(file).Decode()
		if err != nil {
			return "", fmt.Errorf("parse vCard: %w", err)
		}
		if card.Get(vcard.FieldVersion) == nil {
			return "", errors.New("vCard has no VERSION property")
		}
		formattedName := card.Get(vcard.FieldFormattedName)
		if formattedName == nil || strings.TrimSpace(formattedName.Value) == "" {
			return "", errors.New("vCard has no FN property")
		}
		return formattedName.Value, nil
	}
	calendar, err := ical.NewDecoder(file).Decode()
	if err != nil {
		return "", fmt.Errorf("parse iCalendar: %w", err)
	}
	if calendar.Props.Get(ical.PropVersion) == nil {
		return "", errors.New("iCalendar has no VERSION property")
	}
	for _, component := range calendar.Children {
		if summary := component.Props.Get(ical.PropSummary); summary != nil {
			return summary.Value, nil
		}
	}
	return "", nil
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}
func equalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func objectKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}
func safeID(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func writeJSON(path string, value any) error {
	return atomicfile.Write(path, 0o600, func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	})
}
func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
func readMetadata(path string) (Metadata, error) {
	var value Metadata
	err := readJSON(path, &value)
	if err == nil && value.Format != format {
		err = errors.New("unsupported object metadata format")
	}
	return value, err
}
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			written, err := dst.Write(buffer[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}
