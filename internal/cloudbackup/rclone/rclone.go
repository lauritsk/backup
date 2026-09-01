// Package rclone owns the Cloud Backup process boundary to rclone.
package rclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lauritsk/backup/internal/cloudbackup/config"
)

type Runner struct {
	Binary     string
	ConfigPath string
	DataDir    string
}

func (r Runner) Version(ctx context.Context) error {
	return r.run(ctx, []string{"version"})
}

func (r Runner) CheckSource(ctx context.Context, source config.SourceConfig) error {
	args := []string{"lsf", source.Remote, "--max-depth", "1"}
	args = r.withConfig(args)
	return r.run(ctx, args)
}

func (r Runner) Copy(ctx context.Context, source config.SourceConfig, destination string) error {
	if !isRemote(source.Remote) {
		return errors.New("source is not an rclone remote")
	}
	if !beneath(r.DataDir, destination) {
		return errors.New("rclone destination is outside the data directory")
	}
	args := []string{"copy", source.Remote, destination, "--create-empty-src-dirs", "--stats", "0"}
	args = r.withConfig(args)
	for _, pattern := range source.Include {
		args = append(args, "--include", pattern)
	}
	for _, pattern := range source.Exclude {
		args = append(args, "--exclude", pattern)
	}
	if source.BandwidthLimit != "" {
		args = append(args, "--bwlimit", source.BandwidthLimit)
	}
	if source.Transfers > 0 {
		args = append(args, "--transfers", strconv.Itoa(source.Transfers))
	}
	if source.Checkers > 0 {
		args = append(args, "--checkers", strconv.Itoa(source.Checkers))
	}
	return r.run(ctx, args)
}

func (r Runner) withConfig(args []string) []string {
	if r.ConfigPath == "" {
		return args
	}
	return append(args, "--config", r.ConfigPath)
}

func (r Runner) run(ctx context.Context, args []string) error {
	if err := validateInvocation(r.DataDir, args); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, r.Binary, args...)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("rclone %s failed: %w", args[0], err)
	}
	return nil
}

// validateInvocation is a second safety check at the process boundary. New
// commands must be classified here before they can execute.
func validateInvocation(dataDir string, args []string) error {
	if len(args) == 0 {
		return errors.New("empty rclone command")
	}
	switch args[0] {
	case "version":
		if len(args) != 1 {
			return errors.New("unsafe rclone version arguments")
		}
	case "lsf":
		if len(args) < 2 || !isRemote(args[1]) {
			return errors.New("rclone lsf requires a remote source")
		}
	case "copy":
		if len(args) < 3 || !isRemote(args[1]) {
			return errors.New("rclone copy requires a remote source")
		}
		if !beneath(dataDir, args[2]) {
			return errors.New("rclone copy destination is outside the data directory")
		}
	default:
		return fmt.Errorf("rclone command %q is not allowed", args[0])
	}
	return nil
}

func isRemote(value string) bool {
	colon := strings.IndexByte(value, ':')
	return colon > 0 && !strings.ContainsAny(value[:colon], `/\\`) && !strings.ContainsAny(value, "\r\n\x00") && !strings.Contains(value, "://")
}

func beneath(root, name string) bool {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absoluteName, err := filepath.Abs(name)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteName)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if b.Len() < 64<<10 {
		remaining := (64 << 10) - b.Len()
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.Buffer.Write(value)
	}
	return original, nil
}
