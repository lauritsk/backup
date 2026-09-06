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
	"github.com/lauritsk/backup/internal/processenv"
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
	process.Env = processenv.WithoutPrefixes([]string{"RCLONE_CONFIG_"},
		"PGPASSWORD", "MYSQL_PWD", "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE",
		"APPBACKUP_RESTIC_PASSWORD", "APPBACKUP_RESTIC_PASSWORD_FILE",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CLIENT_SECRET", "AZURE_CLIENT_CERTIFICATE_PATH",
		"AZURE_USERNAME", "AZURE_PASSWORD", "AZURE_FEDERATED_TOKEN_FILE", "B2_ACCOUNT", "B2_KEY",
	)
	if err := process.Run(); err != nil {
		if ctxErr := commandCtx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("hook %s failed: %w", filepath.Base(command.Binary), err)
	}
	return nil
}
