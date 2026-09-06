// Package restic owns the Application Backup process boundary to Restic.
package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lauritsk/backup/internal/processenv"
)

type Runner struct {
	Binary       string
	Repository   string
	Password     string
	PasswordFile string
	DataDir      string
	Timeout      time.Duration
}

func (r Runner) Version(ctx context.Context) (string, error) {
	output, err := r.run(ctx, "version")
	if err != nil {
		return "", err
	}
	return firstLine(output), nil
}

func (r Runner) EnsureRepository(ctx context.Context) error {
	configPath := filepath.Join(r.Repository, "config")
	info, err := os.Lstat(configPath)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return errors.New("restic repository config is not a regular file")
		}
		_, err = r.run(ctx, "cat", "config")
		return err
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect restic repository: %w", err)
	default:
		_, err = r.run(ctx, "init")
		return err
	}
}

func (r Runner) CheckRepository(ctx context.Context) error {
	_, err := r.run(ctx, "cat", "config")
	return err
}

func (r Runner) Check(ctx context.Context) error {
	_, err := r.run(ctx, "check")
	return err
}

func (r Runner) Backup(ctx context.Context, paths, tags []string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("restic backup requires at least one path")
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || overlaps(r.Repository, path) || strings.ContainsAny(path, "\r\n\x00") {
			return "", errors.New("restic backup path is unsafe")
		}
	}
	args := []string{"backup", "--json"}
	for _, tag := range tags {
		if invalidValue(tag) {
			return "", errors.New("invalid restic tag")
		}
		args = append(args, "--tag", tag)
	}
	args = append(args, "--")
	args = append(args, paths...)
	return r.runBackup(ctx, args)
}

func (r Runner) Restore(ctx context.Context, snapshot, target string) error {
	if !safeSnapshot(snapshot) || !beneath(r.DataDir, target) || overlaps(r.Repository, target) {
		return errors.New("invalid restic restore destination or snapshot")
	}
	_, err := r.run(ctx, "restore", snapshot, "--target", target)
	return err
}

func (r Runner) List(ctx context.Context, snapshot string, limit, offset int) ([]string, error) {
	if !safeSnapshot(snapshot) || limit < 1 || limit > 1000 || offset < 0 {
		return nil, errors.New("invalid Restic listing request")
	}
	return r.runList(ctx, snapshot, limit, offset)
}

func (r Runner) runList(ctx context.Context, snapshot string, limit, offset int) ([]string, error) {
	var paths []string
	seen := 0
	err := r.stream(ctx, []string{"ls", "--json", snapshot}, func(line []byte) {
		var node struct {
			StructType string `json:"struct_type"`
			Path       string `json:"path"`
		}
		if json.Unmarshal(line, &node) == nil && node.StructType == "node" && node.Path != "" {
			if seen >= offset && len(paths) < limit {
				paths = append(paths, node.Path)
			}
			seen++
		}
	})
	return paths, err
}

func (r Runner) runBackup(ctx context.Context, args []string) (string, error) {
	var snapshot string
	err := r.stream(ctx, args, func(line []byte) {
		var message struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal(line, &message) == nil && message.MessageType == "summary" && message.SnapshotID != "" {
			snapshot = message.SnapshotID
		}
	})
	if err != nil {
		return "", err
	}
	if !safeSnapshot(snapshot) {
		return "", errors.New("restic backup did not report a valid snapshot ID")
	}
	return snapshot, nil
}

func (r Runner) stream(ctx context.Context, args []string, consume func([]byte)) error {
	if err := r.validate(args); err != nil {
		return err
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	command := exec.CommandContext(ctx, r.Binary, args...)
	command.Env = r.environment()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = &cappedBuffer{}
	if err := command.Start(); err != nil {
		return fmt.Errorf("restic %s failed: %w", args[0], err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		consume(scanner.Bytes())
	}
	waitErr := command.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if waitErr != nil {
		return fmt.Errorf("restic %s failed: %w", args[0], waitErr)
	}
	return scanner.Err()
}

func (r Runner) run(ctx context.Context, args ...string) ([]byte, error) {
	if err := r.validate(args); err != nil {
		return nil, err
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	command := exec.CommandContext(ctx, r.Binary, args...)
	command.Env = r.environment()
	var stdout cappedBuffer
	command.Stdout = &stdout
	command.Stderr = &cappedBuffer{}
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("restic %s failed: %w", args[0], err)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("restic %s output exceeded 8 MiB", args[0])
	}
	return stdout.Bytes(), nil
}

func (r Runner) validate(args []string) error {
	if len(args) == 0 || r.Binary == "" || r.Repository != filepath.Join(r.DataDir, "restic") {
		return errors.New("invalid restic invocation")
	}
	switch args[0] {
	case "version", "cat", "init", "check", "backup", "restore", "ls":
	default:
		return fmt.Errorf("restic command %q is not allowed", args[0])
	}
	for _, value := range args {
		if invalidValue(value) {
			return errors.New("restic argument contains a control character")
		}
	}
	return nil
}

func (r Runner) environment() []string {
	result := processenv.Without(
		"RESTIC_REPOSITORY", "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE",
		"APPBACKUP_RESTIC_PASSWORD", "APPBACKUP_RESTIC_PASSWORD_FILE",
	)
	result = append(result, "RESTIC_REPOSITORY="+r.Repository)
	if r.PasswordFile != "" {
		return append(result, "RESTIC_PASSWORD_FILE="+r.PasswordFile)
	}
	return append(result, "RESTIC_PASSWORD="+r.Password)
}

func invalidValue(value string) bool { return value == "" || strings.ContainsAny(value, "\r\n\x00") }
func safeSnapshot(value string) bool {
	return !invalidValue(value) && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, `/\\`)
}
func beneath(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func overlaps(first, second string) bool {
	return first == second || beneath(first, second) || beneath(second, first)
}
func firstLine(value []byte) string {
	line := strings.TrimSpace(string(value))
	if index := strings.IndexByte(line, '\n'); index >= 0 {
		line = line[:index]
	}
	if len(line) > 512 {
		line = line[:512]
	}
	return line
}

type cappedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if b.Len() < 8<<20 {
		remaining := (8 << 20) - b.Len()
		if len(value) > remaining {
			value = value[:remaining]
			b.truncated = true
		}
		_, _ = b.Buffer.Write(value)
	} else if len(value) > 0 {
		b.truncated = true
	}
	return original, nil
}
