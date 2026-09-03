// Package database owns native database dump and verification processes.
package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lauritsk/backup/internal/appbackup/config"
	"github.com/lauritsk/backup/internal/atomicfile"
)

type Runner struct{ DataDir string }

func (Runner) Version(ctx context.Context, database config.DatabaseConfig) (string, error) {
	output, err := run(ctx, database, database.Binary, []string{"--version"}, nil)
	if err != nil {
		return "", err
	}
	if database.VerifyCommand == nil && database.RestoreBinary != database.Binary {
		if _, err := run(ctx, database, database.RestoreBinary, []string{"--version"}, io.Discard); err != nil {
			return "", err
		}
	}
	value := strings.TrimSpace(string(output))
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	if len(value) > 512 {
		value = value[:512]
	}
	return value, nil
}

func (r Runner) Dump(ctx context.Context, database config.DatabaseConfig, destination string) error {
	stagingRoot := filepath.Join(r.DataDir, "staging")
	relative, relErr := filepath.Rel(stagingRoot, destination)
	if r.DataDir == "" || !filepath.IsAbs(destination) || relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("database dump destination must be beneath the staging directory")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	switch database.Type {
	case "postgresql":
		return atomicfile.Write(destination, 0o600, func(writer io.Writer) error {
			_, err := run(ctx, database, database.Binary, append(postgresArgs(database), "--format=custom"), writer)
			return err
		})
	case "mysql", "mariadb":
		return atomicfile.Write(destination, 0o600, func(writer io.Writer) error {
			_, err := run(ctx, database, database.Binary, append(mysqlArgs(database), "--single-transaction", "--routines", "--events", database.Name), writer)
			return err
		})
	case "sqlite":
		return sqliteBackup(ctx, database, destination)
	default:
		return fmt.Errorf("unsupported database type %q", database.Type)
	}
}

func (Runner) VerifyDump(ctx context.Context, database config.DatabaseConfig, dump string) (string, error) {
	if database.VerifyCommand != nil {
		args := make([]string, len(database.VerifyCommand.Args))
		for i, arg := range database.VerifyCommand.Args {
			args[i] = strings.ReplaceAll(arg, "{dump}", dump)
		}
		verifyCtx, cancel := context.WithTimeout(ctx, database.VerifyCommand.Timeout.Duration)
		defer cancel()
		_, err := run(verifyCtx, database, database.VerifyCommand.Binary, args, io.Discard)
		return "passed", err
	}
	switch database.Type {
	case "postgresql":
		_, err := run(ctx, database, database.RestoreBinary, []string{"--list", dump}, io.Discard)
		if err != nil {
			return "failed", err
		}
		return "unknown", nil
	case "sqlite":
		output, err := run(ctx, database, database.RestoreBinary, []string{dump, "PRAGMA quick_check;"}, nil)
		if err != nil {
			return "passed", err
		}
		if strings.TrimSpace(string(output)) != "ok" {
			return "passed", errors.New("SQLite quick_check did not return ok")
		}
		return "passed", nil
	case "mysql", "mariadb":
		return "unknown", nil
	default:
		return "passed", fmt.Errorf("unsupported database type %q", database.Type)
	}
}

func (Runner) Check(ctx context.Context, database config.DatabaseConfig) error {
	if database.VerifyCommand == nil && (database.Type == "postgresql" || database.Type == "sqlite") {
		if _, err := run(ctx, database, database.RestoreBinary, []string{"--version"}, io.Discard); err != nil {
			return err
		}
	}
	switch database.Type {
	case "postgresql":
		_, err := run(ctx, database, database.Binary, append(postgresArgs(database), "--schema-only", "--no-owner"), io.Discard)
		return err
	case "mysql", "mariadb":
		_, err := run(ctx, database, database.Binary, append(mysqlArgs(database), "--no-data", database.Name), io.Discard)
		return err
	case "sqlite":
		output, err := run(ctx, database, database.Binary, []string{database.Path, "PRAGMA quick_check;"}, nil)
		if err == nil && strings.TrimSpace(string(output)) != "ok" {
			return errors.New("SQLite quick_check did not return ok")
		}
		return err
	default:
		return fmt.Errorf("unsupported database type %q", database.Type)
	}
}

func postgresArgs(database config.DatabaseConfig) []string {
	var args []string
	if database.Host != "" {
		args = append(args, "--host", database.Host)
	}
	if database.Port != 0 {
		args = append(args, "--port", strconv.Itoa(database.Port))
	}
	if database.User != "" {
		args = append(args, "--username", database.User)
	}
	return append(args, "--dbname", database.Name)
}
func mysqlArgs(database config.DatabaseConfig) []string {
	var args []string
	if database.Host != "" {
		args = append(args, "--host", database.Host)
	}
	if database.Port != 0 {
		args = append(args, "--port", strconv.Itoa(database.Port))
	}
	if database.User != "" {
		args = append(args, "--user", database.User)
	}
	return args
}

func sqliteBackup(ctx context.Context, database config.DatabaseConfig, destination string) error {
	file, err := os.CreateTemp(filepath.Dir(destination), ".appbackup-sqlite-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	defer os.Remove(temporary)
	if err := os.Chmod(temporary, 0o600); err != nil {
		return err
	}
	quoted := strings.ReplaceAll(temporary, "'", "''")
	if _, err := run(ctx, database, database.Binary, []string{database.Path, ".backup '" + quoted + "'"}, io.Discard); err != nil {
		return err
	}
	file, err = os.OpenFile(temporary, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func run(ctx context.Context, database config.DatabaseConfig, binary string, args []string, stdout io.Writer) ([]byte, error) {
	if binary == "" || strings.ContainsAny(binary, "\r\n\x00") {
		return nil, errors.New("invalid database binary")
	}
	for _, arg := range args {
		if strings.ContainsAny(arg, "\r\n\x00") {
			return nil, errors.New("database argument contains a control character")
		}
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = databaseEnvironment(database)
	var output limitedBuffer
	if stdout == nil {
		command.Stdout = &output
	} else {
		command.Stdout = stdout
	}
	command.Stderr = &limitedBuffer{}
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("database command %s failed: %w", filepath.Base(binary), err)
	}
	return output.Bytes(), nil
}

func databaseEnvironment(database config.DatabaseConfig) []string {
	result := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "PGPASSWORD=") && !strings.HasPrefix(item, "MYSQL_PWD=") {
			result = append(result, item)
		}
	}
	if database.ResolvedPassword != "" {
		if database.Type == "postgresql" {
			result = append(result, "PGPASSWORD="+database.ResolvedPassword)
		}
		if database.Type == "mysql" || database.Type == "mariadb" {
			result = append(result, "MYSQL_PWD="+database.ResolvedPassword)
		}
	}
	return result
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if b.Len() < 1<<20 {
		remaining := (1 << 20) - b.Len()
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.Buffer.Write(value)
	}
	return original, nil
}
