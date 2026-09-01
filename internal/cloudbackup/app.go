// Package cloudbackup is the composition root for the Cloud Backup binary.
package cloudbackup

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/cloudbackup/config"
	"github.com/lauritsk/backup/internal/cloudbackup/model"
	"github.com/lauritsk/backup/internal/logging"
)

const usage = `Usage:
  cloudbackup [global options] <command> [options]

Commands:
  serve             Run the HTTP API and interval scheduler
  backup            Acquire configured sources with rclone
  browse            Read acquired source and file metadata
  verify            Verify acquired files against recorded hashes
  restore           Export selected files beneath /data/restores
  config validate   Validate configuration without network access
  config show       Print effective configuration with secrets redacted
  check             Run storage, rclone, and source diagnostics
  version           Print build information
  help              Print this help

Global options:
  --config PATH      JSON file, default /etc/cloudbackup/config.json
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
	global := flag.NewFlagSet("cloudbackup", flag.ContinueOnError)
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
	command := remaining[0]
	commandArgs := remaining[1:]
	if command == "help" || command == "-h" || command == "--help" {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if command == "version" {
		if len(commandArgs) != 0 {
			fmt.Fprintln(stderr, "cloudbackup: version does not accept arguments")
			return 2
		}
		fmt.Fprintln(stdout, info.Format("cloudbackup"))
		return 0
	}
	switch command {
	case "serve", "backup", "browse", "verify", "restore", "config", "check":
	default:
		fmt.Fprintf(stderr, "cloudbackup: unknown command %q\n", command)
		return 2
	}
	cfg, err := config.Load(config.Overrides{ConfigPath: configPath.pointer(), Listen: listen.pointer(), LogLevel: logLevel.pointer(), LogFormat: logFormat.pointer()})
	if err != nil {
		fmt.Fprintln(stderr, "cloudbackup:", err)
		return 2
	}
	if command == "config" {
		return runConfig(commandArgs, cfg, stdout, stderr)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "cloudbackup: invalid configuration:", err)
		return 2
	}
	logger, err := logging.New(cfg.Log.Level, cfg.Log.Format, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "cloudbackup:", err)
		return 2
	}
	service, err := OpenService(ctx, cfg, ServiceOptions{Logger: logger})
	if err != nil {
		fmt.Fprintln(stderr, "cloudbackup:", err)
		return 1
	}
	defer service.Close()
	switch command {
	case "serve":
		if len(commandArgs) != 0 {
			fmt.Fprintln(stderr, "cloudbackup: serve does not accept arguments")
			return 2
		}
		if err := service.Serve(ctx, info); err != nil {
			fmt.Fprintln(stderr, "cloudbackup: serve:", err)
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
			fmt.Fprintln(stderr, "cloudbackup: check does not accept arguments")
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
		fmt.Fprintln(stderr, "usage: cloudbackup config <validate|show>")
		return 2
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "cloudbackup: invalid configuration:", err)
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
		fmt.Fprintf(stderr, "cloudbackup: unknown config command %q\n", args[0])
		return 2
	}
}
func runBackup(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := commandFlags("backup", stderr)
	var sources stringList
	flags.Var(&sources, "source", "source ID, repeatable")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	run, err := service.Backup(ctx, model.BackupRequest{Sources: sources})
	writeJSONOutput(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "cloudbackup: backup:", err)
		return 1
	}
	return 0
}
func runVerify(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := commandFlags("verify", stderr)
	source := flags.String("source", "", "source ID")
	path := flags.String("path", "", "single relative file path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	run, err := service.Verify(ctx, model.VerifyRequest{SourceID: *source, Path: *path})
	writeJSONOutput(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "cloudbackup: verify:", err)
		return 1
	}
	return 0
}
func runRestore(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := commandFlags("restore", stderr)
	source := flags.String("source", "", "source ID")
	var paths stringList
	flags.Var(&paths, "path", "relative path, repeatable")
	confirm := flags.Bool("yes", false, "confirm local materialization")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	run, err := service.Restore(ctx, model.RestoreRequest{SourceID: *source, Paths: paths, Confirm: *confirm})
	writeJSONOutput(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "cloudbackup: restore:", err)
		return 1
	}
	return 0
}
func runBrowse(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cloudbackup browse <sources|files|file|manifests|manifest> [options]")
		return 2
	}
	switch args[0] {
	case "sources":
		if len(args) != 1 {
			return 2
		}
		value, err := service.ListSources(ctx)
		return output(stdout, stderr, value, err)
	case "files":
		flags := commandFlags("browse files", stderr)
		source := flags.String("source", "", "source ID")
		prefix := flags.String("prefix", "", "path prefix")
		limit := flags.Int("limit", 100, "maximum rows")
		offset := flags.Int("offset", 0, "rows to skip")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *limit < 1 || *limit > 1000 || *offset < 0 {
			return 2
		}
		value, err := service.ListFiles(ctx, *source, *prefix, *limit, *offset)
		return output(stdout, stderr, value, err)
	case "manifests":
		flags := commandFlags("browse manifests", stderr)
		source := flags.String("source", "", "source ID")
		limit := flags.Int("limit", 100, "maximum rows")
		offset := flags.Int("offset", 0, "rows to skip")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *limit < 1 || *limit > 1000 || *offset < 0 {
			return 2
		}
		value, err := service.ListManifests(ctx, *source, *limit, *offset)
		return output(stdout, stderr, value, err)
	case "manifest":
		flags := commandFlags("browse manifest", stderr)
		source := flags.String("source", "", "source ID")
		runID := flags.String("run", "", "backup run ID")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *source == "" || *runID == "" {
			return 2
		}
		value, err := service.GetManifest(ctx, *source, *runID)
		return output(stdout, stderr, value, err)
	case "file":
		flags := commandFlags("browse file", stderr)
		source := flags.String("source", "", "source ID")
		path := flags.String("path", "", "relative path")
		raw := flags.Bool("raw", false, "write file content")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *source == "" || *path == "" {
			return 2
		}
		if !*raw {
			value, err := service.GetFile(ctx, *source, *path)
			return output(stdout, stderr, value, err)
		}
		_, file, err := service.OpenFile(ctx, *source, *path)
		if err != nil {
			fmt.Fprintln(stderr, "cloudbackup:", err)
			return 1
		}
		defer file.Close()
		if _, err := io.Copy(stdout, file); err != nil {
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "cloudbackup: unknown browse command %q\n", args[0])
		return 2
	}
}

func output(stdout, stderr io.Writer, value any, err error) int {
	if err != nil {
		fmt.Fprintln(stderr, "cloudbackup:", err)
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
