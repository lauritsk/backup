// Package appbackup is the composition root for the Application Backup binary.
package appbackup

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/lauritsk/backup/internal/appbackup/config"
	"github.com/lauritsk/backup/internal/appbackup/model"
	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/logging"
)

const usage = `Usage:
  appbackup [global options] <command> [options]

Commands:
  serve             Run the HTTP API and interval scheduler
  backup            Create application recovery points
  browse            Read applications, recovery points, and snapshot contents
  verify            Verify recovery points and database dumps
  restore           Materialize a recovery point beneath /data/restores
  config validate   Validate configuration without external commands
  config show       Print effective configuration with secrets redacted
  check             Run storage, Restic, database, and engine diagnostics
  version           Print build information
  help              Print this help

Global options:
  --config PATH      JSON file, default /etc/appbackup/config.json
  --listen ADDRESS   Override server.listen
  --log-level LEVEL  Override log.level
  --log-format TYPE  Override log.format

Global options must appear before the command.
`

func Run(args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	return RunContext(context.Background(), args, stdout, stderr, info)
}
func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	global := flag.NewFlagSet("appbackup", flag.ContinueOnError)
	global.SetOutput(stderr)
	var configPath, listen, logLevel, logFormat optionalString
	global.Var(&configPath, "config", "JSON configuration path")
	global.Var(&listen, "listen", "HTTP listen address")
	global.Var(&logLevel, "log-level", "log level")
	global.Var(&logFormat, "log-format", "log format")
	global.Usage = func() { _, _ = io.WriteString(stderr, usage) }
	if err := global.Parse(args); err != nil {
		return 2
	}
	remaining := global.Args()
	if len(remaining) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	command, commandArgs := remaining[0], remaining[1:]
	if command == "help" || command == "-h" || command == "--help" {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if command == "version" {
		if len(commandArgs) != 0 {
			fmt.Fprintln(stderr, "appbackup: version does not accept arguments")
			return 2
		}
		fmt.Fprintln(stdout, info.Format("appbackup"))
		return 0
	}
	switch command {
	case "serve", "backup", "browse", "verify", "restore", "config", "check":
	default:
		fmt.Fprintf(stderr, "appbackup: unknown command %q\n", command)
		return 2
	}
	cfg, err := config.Load(config.Overrides{ConfigPath: configPath.pointer(), Listen: listen.pointer(), LogLevel: logLevel.pointer(), LogFormat: logFormat.pointer()})
	if err != nil {
		fmt.Fprintln(stderr, "appbackup:", err)
		return 2
	}
	if command == "config" {
		return runConfig(commandArgs, cfg, stdout, stderr)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "appbackup: invalid configuration:", err)
		return 2
	}
	logger, err := logging.New(cfg.Log.Level, cfg.Log.Format, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup:", err)
		return 2
	}
	service, err := OpenService(ctx, cfg, ServiceOptions{Logger: logger})
	if err != nil {
		fmt.Fprintln(stderr, "appbackup:", err)
		return 1
	}
	defer service.Close()
	switch command {
	case "serve":
		if len(commandArgs) != 0 {
			fmt.Fprintln(stderr, "appbackup: serve does not accept arguments")
			return 2
		}
		if err := service.Serve(ctx, info); err != nil {
			fmt.Fprintln(stderr, "appbackup: serve:", err)
			return 1
		}
		return 0
	case "backup":
		return runBackup(ctx, service, commandArgs, stdout, stderr)
	case "browse":
		return runBrowse(ctx, service, commandArgs, stdout, stderr)
	case "verify":
		return runVerify(ctx, service, commandArgs, stdout, stderr)
	case "restore":
		return runRestore(ctx, service, commandArgs, stdout, stderr)
	case "check":
		if len(commandArgs) != 0 {
			return 2
		}
		report := service.Check(ctx)
		writeJSONOutput(stdout, report)
		if report.Status != "ok" {
			return 1
		}
		return 0
	}
	return 2
}
func runConfig(args []string, cfg config.Config, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: appbackup config <validate|show>")
		return 2
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "appbackup: invalid configuration:", err)
		return 2
	}
	switch args[0] {
	case "validate":
		fmt.Fprintln(stdout, "configuration is valid")
		return 0
	case "show":
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(cfg.RedactedCopy()); err != nil {
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "appbackup: unknown config command %q\n", args[0])
		return 2
	}
}
func runBackup(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := commandFlags("backup", stderr)
	var applications stringList
	flags.Var(&applications, "application", "application ID, repeatable")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	run, err := service.Backup(ctx, model.BackupRequest{Applications: applications})
	writeJSONOutput(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup: backup:", err)
		return 1
	}
	return 0
}
func runVerify(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := commandFlags("verify", stderr)
	point := flags.String("recovery-point", "", "recovery point ID")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	run, err := service.Verify(ctx, model.VerifyRequest{RecoveryPointID: *point})
	writeJSONOutput(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup: verify:", err)
		return 1
	}
	return 0
}
func runRestore(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := commandFlags("restore", stderr)
	point := flags.String("recovery-point", "", "recovery point ID")
	confirm := flags.Bool("yes", false, "confirm local materialization")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	run, err := service.Restore(ctx, model.RestoreRequest{RecoveryPointID: *point, Confirm: *confirm})
	writeJSONOutput(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup: restore:", err)
		return 1
	}
	return 0
}
func runBrowse(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: appbackup browse <applications|recovery-points|recovery-point|contents>")
		return 2
	}
	switch args[0] {
	case "applications":
		if len(args) != 1 {
			return 2
		}
		value, err := service.ListApplications(ctx)
		return output(stdout, stderr, value, err)
	case "recovery-points":
		flags := commandFlags("browse recovery-points", stderr)
		application := flags.String("application", "", "application ID")
		limit := flags.Int("limit", 100, "maximum rows")
		offset := flags.Int("offset", 0, "rows to skip")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *limit < 1 || *limit > 1000 || *offset < 0 {
			return 2
		}
		value, err := service.ListRecoveryPoints(ctx, *application, *limit, *offset)
		return output(stdout, stderr, value, err)
	case "recovery-point":
		flags := commandFlags("browse recovery-point", stderr)
		id := flags.String("id", "", "recovery point ID")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *id == "" {
			return 2
		}
		value, err := service.GetRecoveryPoint(ctx, *id)
		return output(stdout, stderr, value, err)
	case "contents":
		flags := commandFlags("browse contents", stderr)
		id := flags.String("id", "", "recovery point ID")
		limit := flags.Int("limit", 100, "maximum paths")
		offset := flags.Int("offset", 0, "paths to skip")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *id == "" || *limit < 1 || *limit > 1000 || *offset < 0 {
			return 2
		}
		value, err := service.ListRecoveryPointContents(ctx, *id, *limit, *offset)
		return output(stdout, stderr, value, err)
	default:
		fmt.Fprintf(stderr, "appbackup: unknown browse command %q\n", args[0])
		return 2
	}
}
func output(stdout, stderr io.Writer, value any, err error) int {
	if err != nil {
		fmt.Fprintln(stderr, "appbackup:", err)
		return 1
	}
	writeJSONOutput(stdout, value)
	return 0
}
func writeJSONOutput(output io.Writer, value any) {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
func commandFlags(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

type optionalString struct {
	value string
	set   bool
}

func (v *optionalString) String() string         { return v.value }
func (v *optionalString) Set(value string) error { v.value = value; v.set = true; return nil }
func (v *optionalString) pointer() *string {
	if !v.set {
		return nil
	}
	return &v.value
}

type stringList []string

func (v *stringList) String() string { return strings.Join(*v, ",") }
func (v *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value cannot be empty")
	}
	*v = append(*v, value)
	return nil
}
