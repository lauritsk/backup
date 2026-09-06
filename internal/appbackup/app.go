// Package appbackup is the composition root for the Application Backup binary.
package appbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/lauritsk/backup/internal/appbackup/catalog"
	"github.com/lauritsk/backup/internal/appbackup/config"
	"github.com/lauritsk/backup/internal/appbackup/model"
	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/cli"
	"github.com/lauritsk/backup/internal/logging"
	runmodel "github.com/lauritsk/backup/internal/run"
)

const usage = `Usage:
  appbackup [global options] <command> [options]

Commands:
  backup            Create application recovery points
  status            Show the latest backup and configured applications
  list              List applications, recovery points, snapshot contents, or runs
  show              Show one recovery point or run
  verify            Deep-check Restic and verify the latest recovery point per app
  export            Materialize a recovery point beneath data_dir/exports
  repair            Reconcile all recovery-point manifests with the catalog
  config init       Print or write a minimal configuration
  config validate   Validate configuration without running external commands
  config show       Print effective configuration with secrets redacted
  check             Run local storage, Restic, database, and hook diagnostics
  version           Print build information
  help              Print this help

Global options may appear before or after the command:
  --config PATH      JSON file, default /etc/appbackup/config.json
  --data-dir PATH    State directory, default /data
  --log-level LEVEL  debug, info, warn, or error
  --log-format TYPE  json or text
  --json             Emit machine-readable command output
`

const initialConfig = `{
  "restic": {
    "password_file": "/run/secrets/appbackup_restic_password"
  },
  "applications": [
    {
      "id": "example",
      "paths": ["/srv/example"]
    }
  ]
}
`

func Run(args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	return RunContext(context.Background(), args, stdout, stderr, info)
}

func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	globals, remaining, err := cli.ExtractGlobals(args)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup:", err)
		return 2
	}
	if len(remaining) == 0 {
		io.WriteString(stderr, usage)
		return 2
	}
	command, commandArgs := remaining[0], remaining[1:]
	if command == "help" || command == "-h" || command == "--help" {
		io.WriteString(stdout, usage)
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
	if command == "config" && len(commandArgs) > 0 && commandArgs[0] == "init" {
		return runConfigInit(commandArgs[1:], stdout, stderr)
	}
	switch command {
	case "backup", "status", "list", "show", "verify", "export", "repair", "config", "check":
	default:
		fmt.Fprintf(stderr, "appbackup: unknown command %q\n", command)
		return 2
	}

	cfg, err := config.Load(config.Overrides{
		ConfigPath: globals.ConfigPath,
		DataDir:    globals.DataDir,
		LogLevel:   globals.LogLevel,
		LogFormat:  globals.LogFormat,
	})
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
	service, err := OpenService(ctx, cfg, ServiceOptions{Logger: logger, DeferFullReconcile: command == "repair"})
	if err != nil {
		fmt.Fprintln(stderr, "appbackup:", err)
		return 1
	}
	defer service.Close()

	switch command {
	case "backup":
		return runBackup(ctx, service, commandArgs, globals.JSON, stdout, stderr)
	case "status":
		return runStatus(ctx, service, commandArgs, globals.JSON, stdout, stderr)
	case "list":
		return runList(ctx, service, commandArgs, globals.JSON, stdout, stderr)
	case "show":
		return runShow(ctx, service, commandArgs, stdout, stderr)
	case "verify":
		return runVerify(ctx, service, commandArgs, globals.JSON, stdout, stderr)
	case "export":
		return runExport(ctx, service, commandArgs, globals.JSON, stdout, stderr)
	case "repair":
		return runRepair(ctx, service, commandArgs, globals.JSON, stdout, stderr)
	case "check":
		return runCheck(ctx, service, commandArgs, globals.JSON, stdout, stderr)
	default:
		return 2
	}
}

func runConfigInit(args []string, stdout, stderr io.Writer) int {
	flags := commandFlags("config init", stderr)
	output := flags.String("output", "", "new configuration path, or - for stdout")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: appbackup config init [--output PATH]")
		return 2
	}
	if err := cli.WriteNewFile(stdout, *output, []byte(initialConfig)); err != nil {
		fmt.Fprintln(stderr, "appbackup:", err)
		return 1
	}
	return 0
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
		if err := cli.WriteJSON(stdout, cfg.RedactedCopy()); err != nil {
			fmt.Fprintln(stderr, "appbackup: encode configuration:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "appbackup: unknown config command %q\n", args[0])
		return 2
	}
}

func runBackup(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	flags := commandFlags("backup", stderr)
	var applications stringList
	flags.Var(&applications, "application", "application ID, repeatable")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "appbackup: backup accepts flags only")
		return 2
	}
	record, err := service.Backup(ctx, model.BackupRequest{Applications: applications})
	writeRun(stdout, record, jsonOutput)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup: backup:", service.cleanError(err))
		return 1
	}
	return 0
}

func runVerify(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	flags := commandFlags("verify", stderr)
	point := flags.String("id", "", "one recovery point ID")
	application := flags.String("application", "", "newest recovery point for one application")
	all := flags.Bool("all", false, "verify every recovery point")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	selectors := 0
	for _, selected := range []bool{*point != "", *application != "", *all} {
		if selected {
			selectors++
		}
	}
	if flags.NArg() != 0 || selectors > 1 {
		fmt.Fprintln(stderr, "appbackup: verify accepts one of --id, --application, or --all")
		return 2
	}
	record, err := service.Verify(ctx, model.VerifyRequest{RecoveryPointID: *point, ApplicationID: *application, All: *all})
	writeRun(stdout, record, jsonOutput)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup: verify:", service.cleanError(err))
		return 1
	}
	return 0
}

func runExport(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	flags := commandFlags("export", stderr)
	point := flags.String("id", "", "recovery point ID")
	confirm := flags.Bool("confirm", false, "confirm materialization")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 || *point == "" {
		fmt.Fprintln(stderr, "usage: appbackup export --id ID --confirm")
		return 2
	}
	record, err := service.Export(ctx, model.ExportRequest{RecoveryPointID: *point, Confirm: *confirm})
	writeRun(stdout, record, jsonOutput)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup: export:", service.cleanError(err))
		return 1
	}
	return 0
}

func runRepair(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	flags := commandFlags("repair", stderr)
	confirm := flags.Bool("confirm", false, "confirm full catalog reconciliation")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 || !*confirm {
		fmt.Fprintln(stderr, "usage: appbackup repair --confirm")
		return 2
	}
	if err := service.Repair(ctx); err != nil {
		fmt.Fprintln(stderr, "appbackup: repair:", service.cleanError(err))
		return 1
	}
	if jsonOutput {
		_ = cli.WriteJSON(stdout, map[string]string{"status": "repaired"})
	} else {
		fmt.Fprintln(stdout, "catalog reconciliation complete")
	}
	return 0
}

func runCheck(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "appbackup: check does not accept arguments")
		return 2
	}
	report := service.Check(ctx)
	if jsonOutput {
		_ = cli.WriteJSON(stdout, report)
	} else {
		writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		for _, check := range report.Checks {
			fmt.Fprintf(writer, "%s\t%s\t%s\n", check.Status, check.Name, check.Message)
		}
		writer.Flush()
	}
	if report.Status != "ok" {
		return 1
	}
	return 0
}

func runStatus(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "appbackup: status does not accept arguments")
		return 2
	}
	applications, err := service.ListApplications(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup: status:", service.cleanError(err))
		return 1
	}
	runs, err := service.ListRuns(ctx, 100, 0)
	if err != nil {
		fmt.Fprintln(stderr, "appbackup: status:", service.cleanError(err))
		return 1
	}
	var latest *catalog.Run
	for index := range runs {
		if runs[index].Operation == runmodel.OperationBackup {
			latest = &runs[index]
			break
		}
	}
	report := struct {
		Applications []model.Application `json:"applications"`
		LatestBackup *catalog.Run        `json:"latest_backup,omitempty"`
	}{Applications: applications, LatestBackup: latest}
	if jsonOutput {
		_ = cli.WriteJSON(stdout, report)
		return 0
	}
	if latest == nil {
		fmt.Fprintln(stdout, "last backup: never")
	} else {
		fmt.Fprintf(stdout, "last backup: %s at %s, run %s\n", latest.Status, latest.RequestedAt.Format("2006-01-02 15:04:05Z07:00"), latest.ID)
	}
	for _, application := range applications {
		state := "enabled"
		if application.Disabled {
			state = "disabled"
		}
		fmt.Fprintf(stdout, "%s: %s, recovery points %d\n", application.ID, state, application.RecoveryPoints)
	}
	return 0
}

func runList(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: appbackup list <applications|recovery-points|contents|runs>")
		return 2
	}
	switch args[0] {
	case "applications":
		if len(args) != 1 {
			return 2
		}
		values, err := service.ListApplications(ctx)
		if err != nil {
			return outputError(stderr, service, err)
		}
		if jsonOutput {
			_ = cli.WriteJSON(stdout, values)
		} else {
			for _, value := range values {
				fmt.Fprintf(stdout, "%s\trecovery_points=%d\n", value.ID, value.RecoveryPoints)
			}
		}
		return 0
	case "recovery-points":
		flags := commandFlags("list recovery-points", stderr)
		application := flags.String("application", "", "application ID")
		limit := flags.Int("limit", 100, "maximum rows")
		offset := flags.Int("offset", 0, "rows to skip")
		if err := flags.Parse(args[1:]); err != nil {
			return flagResult(err)
		}
		if flags.NArg() != 0 || *limit < 1 || *limit > 1000 || *offset < 0 {
			return 2
		}
		values, err := service.ListRecoveryPoints(ctx, *application, *limit, *offset)
		if err != nil {
			return outputError(stderr, service, err)
		}
		if jsonOutput {
			_ = cli.WriteJSON(stdout, values)
		} else {
			for _, value := range values {
				fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", value.ID, value.ApplicationID, value.Status, value.StartedAt.Format("2006-01-02T15:04:05Z"))
			}
		}
		return 0
	case "contents":
		flags := commandFlags("list contents", stderr)
		id := flags.String("id", "", "recovery point ID")
		limit := flags.Int("limit", 100, "maximum paths")
		offset := flags.Int("offset", 0, "paths to skip")
		if err := flags.Parse(args[1:]); err != nil {
			return flagResult(err)
		}
		if flags.NArg() != 0 || *id == "" || *limit < 1 || *limit > 1000 || *offset < 0 {
			return 2
		}
		values, err := service.ListRecoveryPointContents(ctx, *id, *limit, *offset)
		if err != nil {
			return outputError(stderr, service, err)
		}
		if jsonOutput {
			_ = cli.WriteJSON(stdout, values)
		} else {
			for _, value := range values {
				fmt.Fprintln(stdout, value)
			}
		}
		return 0
	case "runs":
		flags := commandFlags("list runs", stderr)
		limit := flags.Int("limit", 100, "maximum rows")
		offset := flags.Int("offset", 0, "rows to skip")
		if err := flags.Parse(args[1:]); err != nil {
			return flagResult(err)
		}
		if flags.NArg() != 0 || *limit < 1 || *limit > 1000 || *offset < 0 {
			return 2
		}
		values, err := service.ListRuns(ctx, *limit, *offset)
		if err != nil {
			return outputError(stderr, service, err)
		}
		if jsonOutput {
			_ = cli.WriteJSON(stdout, values)
		} else {
			for _, value := range values {
				fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", value.ID, value.Operation, value.Status, value.RequestedAt.Format("2006-01-02T15:04:05Z"))
			}
		}
		return 0
	default:
		fmt.Fprintf(stderr, "appbackup: unknown list target %q\n", args[0])
		return 2
	}
}

func runShow(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: appbackup show <recovery-point|run> --id ID")
		return 2
	}
	flags := commandFlags("show "+args[0], stderr)
	id := flags.String("id", "", "record ID")
	if err := flags.Parse(args[1:]); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 || *id == "" {
		return 2
	}
	var value any
	var err error
	switch args[0] {
	case "recovery-point":
		value, err = service.GetRecoveryPoint(ctx, *id)
	case "run":
		value, err = service.GetRun(ctx, *id)
	default:
		fmt.Fprintf(stderr, "appbackup: unknown show target %q\n", args[0])
		return 2
	}
	if err != nil {
		return outputError(stderr, service, err)
	}
	if err := cli.WriteJSON(stdout, value); err != nil {
		fmt.Fprintln(stderr, "appbackup:", err)
		return 1
	}
	return 0
}

func writeRun(output io.Writer, record catalog.Run, jsonOutput bool) {
	if jsonOutput {
		_ = cli.WriteJSON(output, record)
		return
	}
	fmt.Fprintf(output, "%s %s, run %s\n", record.Operation, record.Status, record.ID)
	if record.Error != "" {
		fmt.Fprintln(output, record.Error)
	}
	if len(record.Detail) > 0 && string(record.Detail) != "null" && string(record.Detail) != "{}" {
		var indented bytes.Buffer
		if json.Indent(&indented, record.Detail, "", "  ") == nil {
			fmt.Fprintln(output, indented.String())
		}
	}
}

func outputError(stderr io.Writer, service *Service, err error) int {
	fmt.Fprintln(stderr, "appbackup:", service.cleanError(err))
	return 1
}

func commandFlags(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func flagResult(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value cannot be empty")
	}
	*values = append(*values, value)
	return nil
}
