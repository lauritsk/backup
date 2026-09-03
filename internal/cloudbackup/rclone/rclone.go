// Package rclone provides read-only cloud access through rclone's Go packages.
package rclone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/rclone/rclone/backend/all"
	"github.com/rclone/rclone/fs"
	rcloneconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"
	"golang.org/x/time/rate"

	cloudconfig "github.com/lauritsk/backup/internal/cloudbackup/config"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
)

const maxInventoryBytes = 64 << 20

type Runner struct {
	ConfigPath string
}

var embeddedConfig struct {
	sync.Mutex
	installed bool
	path      string
}

func (r Runner) Version(context.Context) error {
	return r.configure()
}

func (r Runner) CheckSource(ctx context.Context, source cloudconfig.SourceConfig) error {
	remote, err := r.open(ctx, source.Remote)
	if err != nil {
		return fmt.Errorf("open rclone source: %w", err)
	}
	if _, err := remote.List(ctx, ""); err != nil {
		return fmt.Errorf("list rclone source: %w", err)
	}
	return nil
}

func (r Runner) Inventory(ctx context.Context, source cloudconfig.SourceConfig) ([]model.RemoteFile, error) {
	if !isRemote(source.Remote) {
		return nil, errors.New("source is not an rclone remote")
	}
	ctx, err := sourceContext(ctx, source)
	if err != nil {
		return nil, err
	}
	remote, err := r.open(ctx, source.Remote)
	if err != nil {
		return nil, fmt.Errorf("open rclone source: %w", err)
	}
	options := operations.ListJSONOpt{Recurse: true, FilesOnly: true, ShowHash: true}
	var files []model.RemoteFile
	size := 0
	err = operations.ListJSON(ctx, remote, "", &options, func(item *operations.ListJSONItem) error {
		size += len(item.Path) + len(item.Name) + 128
		hashes := make(map[string]string, len(item.Hashes))
		for name, value := range item.Hashes {
			size += len(name) + len(value)
			hashes[name] = value
		}
		if size > maxInventoryBytes {
			return errors.New("rclone inventory exceeds 64 MiB")
		}
		files = append(files, model.RemoteFile{
			Path:    item.Path,
			Size:    item.Size,
			ModTime: item.ModTime.When,
			IsDir:   item.IsDir,
			Hashes:  hashes,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inventory rclone source: %w", err)
	}
	return files, nil
}

func (r Runner) Download(ctx context.Context, source cloudconfig.SourceConfig, path string, destination io.Writer) error {
	if _, err := remoteObject(source.Remote, path); err != nil {
		return err
	}
	ctx, err := sourceContext(ctx, source)
	if err != nil {
		return err
	}
	remote, err := r.open(ctx, source.Remote)
	if err != nil {
		return fmt.Errorf("open rclone source: %w", err)
	}
	object, err := remote.NewObject(ctx, path)
	if err != nil {
		return fmt.Errorf("open remote object: %w", err)
	}
	reader, err := object.Open(ctx)
	if err != nil {
		return fmt.Errorf("read remote object: %w", err)
	}
	var input io.Reader = reader
	if source.BandwidthLimit != "" && !strings.EqualFold(source.BandwidthLimit, "off") {
		limit, err := bandwidthBytes(source.BandwidthLimit)
		if err != nil {
			reader.Close()
			return err
		}
		if limit > 0 {
			input = newLimitedReader(ctx, reader, limit)
		}
	}
	_, copyErr := io.Copy(destination, input)
	closeErr := reader.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("read remote object: %w", err)
	}
	return nil
}

func (r Runner) configure() error {
	embeddedConfig.Lock()
	defer embeddedConfig.Unlock()
	if embeddedConfig.installed {
		if embeddedConfig.path != r.ConfigPath {
			return errors.New("embedded rclone cannot use multiple config paths in one process")
		}
		return nil
	}
	if r.ConfigPath != "" {
		if err := rcloneconfig.SetConfigPath(r.ConfigPath); err != nil {
			return fmt.Errorf("set rclone config path: %w", err)
		}
	}
	configfile.Install()
	embeddedConfig.installed = true
	embeddedConfig.path = r.ConfigPath
	return nil
}

func (r Runner) open(ctx context.Context, name string) (fs.Fs, error) {
	if err := r.configure(); err != nil {
		return nil, err
	}
	return fs.NewFs(ctx, name)
}

func sourceContext(ctx context.Context, source cloudconfig.SourceConfig) (context.Context, error) {
	ctx, settings := fs.AddConfig(ctx)
	if source.Transfers > 0 {
		settings.Transfers = source.Transfers
	}
	if source.Checkers > 0 {
		settings.Checkers = source.Checkers
	}
	options := filter.Opt
	options.IncludeRule = append([]string(nil), source.Include...)
	options.ExcludeRule = append([]string(nil), source.Exclude...)
	configuredFilter, err := filter.NewFilter(&options)
	if err != nil {
		return nil, fmt.Errorf("configure rclone filters: %w", err)
	}
	return filter.ReplaceConfig(ctx, configuredFilter), nil
}

func bandwidthBytes(value string) (int64, error) {
	value = strings.TrimSuffix(strings.TrimSuffix(value, "/s"), "/S")
	var parsed fs.SizeSuffix
	if err := parsed.Set(value); err != nil || parsed < 0 {
		if err == nil {
			err = errors.New("rate cannot be negative")
		}
		return 0, fmt.Errorf("invalid rclone bandwidth limit: %w", err)
	}
	return int64(parsed), nil
}

type limitedReader struct {
	ctx     context.Context
	reader  io.Reader
	limiter *rate.Limiter
	burst   int
}

func newLimitedReader(ctx context.Context, reader io.Reader, bytesPerSecond int64) *limitedReader {
	burst := int64(128 << 10)
	if bytesPerSecond < burst {
		burst = bytesPerSecond
	}
	if burst < 1 {
		burst = 1
	}
	return &limitedReader{
		ctx:     ctx,
		reader:  reader,
		limiter: rate.NewLimiter(rate.Limit(bytesPerSecond), int(burst)),
		burst:   int(burst),
	}
}

func (r *limitedReader) Read(buffer []byte) (int, error) {
	if len(buffer) > r.burst {
		buffer = buffer[:r.burst]
	}
	n, err := r.reader.Read(buffer)
	if n > 0 {
		if waitErr := r.limiter.WaitN(r.ctx, n); waitErr != nil {
			return 0, waitErr
		}
	}
	return n, err
}

func isRemote(value string) bool {
	colon := strings.IndexByte(value, ':')
	return colon > 0 && !strings.ContainsAny(value[:colon], `/\\`) && !strings.ContainsAny(value, "\r\n\x00") && !strings.Contains(value, "://")
}

func remoteObject(remote, objectPath string) (string, error) {
	if !isRemote(remote) || objectPath == "" || strings.HasPrefix(objectPath, "/") || strings.ContainsAny(objectPath, "\\\r\n\x00") {
		return "", errors.New("invalid remote object path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(objectPath)))
	if clean != objectPath || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("invalid remote object path")
	}
	return strings.TrimSuffix(remote, "/") + "/" + objectPath, nil
}
