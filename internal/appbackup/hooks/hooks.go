// Package hooks executes configured application lifecycle commands without a shell.
package hooks

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

func (Runner) Run(ctx context.Context, command config.CommandConfig) error {
	if command.Binary == "" || strings.ContainsAny(command.Binary, "\r\n\x00") {
		return errors.New("invalid hook binary")
	}
	for _, arg := range command.Args {
		if strings.ContainsAny(arg, "\r\n\x00") {
			return errors.New("hook argument contains a control character")
		}
	}
	commandCtx, cancel := context.WithTimeout(ctx, command.Timeout.Duration)
	defer cancel()
	process := exec.CommandContext(commandCtx, command.Binary, command.Args...)
	if err := process.Run(); err != nil {
		if ctxErr := commandCtx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("hook %s failed: %w", filepath.Base(command.Binary), err)
	}
	return nil
}
