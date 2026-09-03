// Package engine performs optional Docker and Podman diagnostics.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lauritsk/backup/internal/appbackup/config"
)

type Runner struct{}

func (Runner) Check(ctx context.Context, engine config.EngineConfig) error {
	if engine.Binary == "" || !filepath.IsAbs(engine.Socket) || strings.ContainsAny(engine.Binary+engine.Socket, "\r\n\x00") {
		return errors.New("invalid container engine configuration")
	}
	var args []string
	switch engine.Type {
	case "docker":
		args = []string{"--host", "unix://" + engine.Socket, "version"}
	case "podman":
		args = []string{"--url", "unix://" + engine.Socket, "version"}
	default:
		return fmt.Errorf("unsupported container engine %q", engine.Type)
	}
	command := exec.CommandContext(ctx, engine.Binary, args...)
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("container engine check failed: %w", err)
	}
	return nil
}
