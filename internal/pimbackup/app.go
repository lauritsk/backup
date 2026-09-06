// Package pimbackup is the composition root for the PIM Backup binary.
package pimbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/cli"
	"github.com/lauritsk/backup/internal/logging"
	"github.com/lauritsk/backup/internal/pimbackup/catalog"
	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/pimbackup/model"
	runmodel "github.com/lauritsk/backup/internal/run"
)

const usage = `Usage:
  pimbackup [global options] <command> [options]

Commands:
  backup            Back up configured PIM accounts
  status            Show the latest backup and account totals
  list              List accounts, mailboxes, messages, collections, objects, or runs
  show              Show one message, object, or run
  verify            Verify stored PIM data
  restore           Restore selected PIM objects to a configured account
  repair            Rebuild the catalog from canonical files
  config init       Print or write a minimal configuration
  config validate   Validate configuration without network access
  config show       Print effective configuration with secrets redacted
  check             Run storage, SQLite, and remote-account diagnostics
  version           Print build information
  help              Print this help

Global options may appear before or after the command:
  --config PATH      JSON file, default /etc/pimbackup/config.json
  --data-dir PATH    State directory, default /data
  --log-level LEVEL  debug, info, warn, or error
  --log-format TYPE  json or text
  --json             Emit machine-readable command output
`

const initialConfig = `{
  "accounts": [
    {
      "id": "personal",
      "protocol": "imap",
      "host": "imap.example.com",
      "username": "person@example.com",
      "password_file": "/run/secrets/pimbackup_password"
    }
  ]
}
`

// Run executes the root command and returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	globals, remaining, err := cli.ExtractGlobals(args)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
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
			fmt.Fprintln(stderr, "pimbackup: version does not accept arguments")
			return 2
		}
		fmt.Fprintln(stdout, info.Format("pimbackup"))
		return 0
	}
	if command == "config" && len(commandArgs) > 0 && commandArgs[0] == "init" {
		return runConfigInit(commandArgs[1:], stdout, stderr)
	}
	switch command {
	case "backup", "status", "list", "show", "verify", "restore", "repair", "config", "check":
	default:
		fmt.Fprintf(stderr, "pimbackup: unknown command %q\n", command)
		return 2
	}

	cfg, err := config.Load(config.Overrides{
		ConfigPath: globals.ConfigPath,
		DataDir:    globals.DataDir,
		LogLevel:   globals.LogLevel,
		LogFormat:  globals.LogFormat,
	})
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
		return 2
	}
	if command == "config" {
		return runConfig(commandArgs, cfg, stdout, stderr)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "pimbackup: invalid configuration:", err)
		return 2
	}
	logger, err := logging.New(cfg.Log.Level, cfg.Log.Format, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
		return 2
	}
	service, err := OpenService(ctx, cfg, ServiceOptions{Logger: logger, DeferFullReconcile: command == "repair"})
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
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
	case "restore":
		return runRestore(ctx, service, commandArgs, globals.JSON, stdout, stderr)
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
		fmt.Fprintln(stderr, "usage: pimbackup config init [--output PATH]")
		return 2
	}
	if err := cli.WriteNewFile(stdout, *output, []byte(initialConfig)); err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
		return 1
	}
	return 0
}

func runConfig(args []string, cfg config.Config, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: pimbackup config <validate|show>")
		return 2
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "pimbackup: invalid configuration:", err)
		return 2
	}
	switch args[0] {
	case "validate":
		fmt.Fprintln(stdout, "configuration is valid")
		return 0
	case "show":
		if err := cli.WriteJSON(stdout, cfg.RedactedCopy()); err != nil {
			fmt.Fprintln(stderr, "pimbackup: encode configuration:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "pimbackup: unknown config command %q\n", args[0])
		return 2
	}
}

func runBackup(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	flags := commandFlags("backup", stderr)
	var accounts stringList
	flags.Var(&accounts, "account", "account ID, repeatable")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "pimbackup: backup accepts flags only")
		return 2
	}
	record, err := service.Backup(ctx, model.BackupRequest{Accounts: accounts})
	writeRun(stdout, record, jsonOutput)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup: backup:", service.cleanError(err))
		return 1
	}
	return 0
}

func runStatus(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "pimbackup: status does not accept arguments")
		return 2
	}
	accounts, err := service.ListAccounts(ctx)
	if err != nil {
		return outputError(stderr, service, err)
	}
	runs, err := service.ListRuns(ctx, 100, 0)
	if err != nil {
		return outputError(stderr, service, err)
	}
	var latest *catalog.Run
	for index := range runs {
		if runs[index].Operation == runmodel.OperationBackup {
			latest = &runs[index]
			break
		}
	}
	report := struct {
		Accounts     []model.Account `json:"accounts"`
		LatestBackup *catalog.Run    `json:"latest_backup,omitempty"`
	}{Accounts: accounts, LatestBackup: latest}
	if jsonOutput {
		_ = cli.WriteJSON(stdout, report)
		return 0
	}
	if latest == nil {
		fmt.Fprintln(stdout, "last backup: never")
	} else {
		fmt.Fprintf(stdout, "last backup: %s at %s, run %s\n", latest.Status, latest.RequestedAt.Format("2006-01-02 15:04:05Z07:00"), latest.ID)
	}
	for _, account := range accounts {
		fmt.Fprintf(stdout, "%s: %s, messages %d, objects %d\n", account.ID, account.Protocol, account.Messages, account.Objects)
	}
	return 0
}

func runVerify(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	flags := commandFlags("verify", stderr)
	account := flags.String("account", "", "account ID")
	messageID := flags.Int64("message-id", 0, "single IMAP message ID")
	objectID := flags.Int64("object-id", 0, "single JMAP, contact, or calendar object ID")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 || *messageID < 0 || *objectID < 0 || *messageID != 0 && *objectID != 0 {
		fmt.Fprintln(stderr, "pimbackup: verify accepts --account and one of --message-id or --object-id")
		return 2
	}
	record, err := service.Verify(ctx, model.VerifyRequest{AccountID: *account, MessageID: *messageID, ObjectID: *objectID})
	writeRun(stdout, record, jsonOutput)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup: verify:", service.cleanError(err))
		return 1
	}
	return 0
}

func runRestore(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	flags := commandFlags("restore", stderr)
	var messageIDs, objectIDs int64List
	flags.Var(&messageIDs, "message-id", "IMAP message ID, repeatable")
	flags.Var(&objectIDs, "object-id", "JMAP, contact, or calendar object ID, repeatable")
	targetAccount := flags.String("target-account", "", "configured destination account")
	targetMailbox := flags.String("target-mailbox", "", "destination IMAP or JMAP mailbox")
	targetCollection := flags.String("target-collection", "", "destination CardDAV or CalDAV collection")
	createMailbox := flags.Bool("create-mailbox", false, "create a missing destination mailbox")
	confirm := flags.Bool("confirm", false, "confirm the remote write")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "pimbackup: restore accepts flags only")
		return 2
	}
	record, err := service.Restore(ctx, model.RestoreRequest{
		MessageIDs: messageIDs, ObjectIDs: objectIDs, TargetAccount: *targetAccount,
		TargetMailbox: *targetMailbox, TargetCollection: *targetCollection,
		CreateMailbox: *createMailbox, Confirm: *confirm,
	})
	writeRun(stdout, record, jsonOutput)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup: restore:", service.cleanError(err))
		return 1
	}
	return 0
}

func runRepair(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	flags := commandFlags("repair", stderr)
	confirm := flags.Bool("confirm", false, "confirm catalog replacement")
	if err := flags.Parse(args); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 || !*confirm {
		fmt.Fprintln(stderr, "usage: pimbackup repair --confirm")
		return 2
	}
	report, err := service.Rebuild(ctx)
	if jsonOutput {
		_ = cli.WriteJSON(stdout, report)
	} else {
		fmt.Fprintf(stdout, "rebuilt %d mailboxes, %d messages, %d collections, %d objects\n", report.Mailboxes, report.Messages, report.Collections, report.Objects)
	}
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup: repair:", service.cleanError(err))
		return 1
	}
	return 0
}

func runCheck(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "pimbackup: check does not accept arguments")
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

func runList(ctx context.Context, service *Service, args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: pimbackup list <accounts|mailboxes|messages|collections|objects|runs>")
		return 2
	}
	target := args[0]
	flags := commandFlags("list "+target, stderr)
	account := flags.String("account", "", "account ID")
	mailbox := flags.String("mailbox", "", "mailbox name")
	collection := flags.String("collection", "", "collection name")
	kind := flags.String("kind", "", "mail, contact, or calendar")
	includeInactive := flags.Bool("include-inactive", false, "include inactive records")
	uidValidity := flags.Uint("uid-validity", 0, "UIDVALIDITY generation")
	limit := flags.Int("limit", 100, "maximum rows")
	offset := flags.Int("offset", 0, "rows to skip")
	if err := flags.Parse(args[1:]); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 || *limit < 1 || *limit > 1000 || *offset < 0 || *uidValidity > uint(^uint32(0)) {
		return 2
	}
	var value any
	var err error
	switch target {
	case "accounts":
		value, err = service.ListAccounts(ctx)
	case "mailboxes":
		value, err = service.ListMailboxes(ctx, *account, *includeInactive)
	case "messages":
		value, err = service.ListMessages(ctx, model.MessageFilter{AccountID: *account, Mailbox: *mailbox, UIDValidity: uint32(*uidValidity), Limit: *limit, Offset: *offset})
	case "collections":
		value, err = service.ListCollections(ctx, *account, *kind, *includeInactive)
	case "objects":
		value, err = service.ListObjects(ctx, model.ObjectFilter{AccountID: *account, Collection: *collection, Kind: *kind, Limit: *limit, Offset: *offset})
	case "runs":
		value, err = service.ListRuns(ctx, *limit, *offset)
	default:
		fmt.Fprintf(stderr, "pimbackup: unknown list target %q\n", target)
		return 2
	}
	if err != nil {
		return outputError(stderr, service, err)
	}
	if jsonOutput {
		_ = cli.WriteJSON(stdout, value)
		return 0
	}
	writeListText(stdout, value)
	return 0
}

func writeListText(output io.Writer, value any) {
	switch values := value.(type) {
	case []model.Account:
		for _, item := range values {
			fmt.Fprintf(output, "%s\t%s\tmessages=%d\tobjects=%d\n", item.ID, item.Protocol, item.Messages, item.Objects)
		}
	case []model.Mailbox:
		for _, item := range values {
			fmt.Fprintf(output, "%d\t%s\t%s\tmessages=%d\n", item.ID, item.AccountID, item.Name, item.Messages)
		}
	case []model.Message:
		for _, item := range values {
			fmt.Fprintf(output, "%d\t%s\t%s\t%d\t%s\n", item.ID, item.AccountID, item.Mailbox, item.Size, item.Subject)
		}
	case []model.Collection:
		for _, item := range values {
			fmt.Fprintf(output, "%d\t%s\t%s\t%s\tobjects=%d\n", item.ID, item.AccountID, item.Kind, item.Name, item.Objects)
		}
	case []model.Object:
		for _, item := range values {
			fmt.Fprintf(output, "%d\t%s\t%s\t%d\t%s\n", item.ID, item.AccountID, item.Kind, item.Size, item.Title)
		}
	case []catalog.Run:
		for _, item := range values {
			fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", item.ID, item.Operation, item.Status, item.RequestedAt.Format("2006-01-02T15:04:05Z"))
		}
	}
}

func runShow(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: pimbackup show <message|object|run> [options]")
		return 2
	}
	flags := commandFlags("show "+args[0], stderr)
	id := flags.String("id", "", "record ID")
	raw := flags.Bool("raw", false, "write original content")
	if err := flags.Parse(args[1:]); err != nil {
		return flagResult(err)
	}
	if flags.NArg() != 0 || *id == "" {
		return 2
	}
	switch args[0] {
	case "message":
		numeric, err := strconv.ParseInt(*id, 10, 64)
		if err != nil || numeric <= 0 {
			return 2
		}
		if !*raw {
			value, err := service.GetMessage(ctx, numeric)
			return writeResult(stdout, stderr, service, value, err)
		}
		_, file, err := service.OpenMessage(ctx, numeric)
		if err != nil {
			return outputError(stderr, service, err)
		}
		defer file.Close()
		if _, err := io.Copy(stdout, file); err != nil {
			return outputError(stderr, service, err)
		}
		return 0
	case "object":
		numeric, err := strconv.ParseInt(*id, 10, 64)
		if err != nil || numeric <= 0 {
			return 2
		}
		if !*raw {
			value, err := service.GetObject(ctx, numeric)
			return writeResult(stdout, stderr, service, value, err)
		}
		_, file, err := service.OpenObject(ctx, numeric)
		if err != nil {
			return outputError(stderr, service, err)
		}
		defer file.Close()
		if _, err := io.Copy(stdout, file); err != nil {
			return outputError(stderr, service, err)
		}
		return 0
	case "run":
		if *raw {
			return 2
		}
		value, err := service.GetRun(ctx, *id)
		return writeResult(stdout, stderr, service, value, err)
	default:
		fmt.Fprintf(stderr, "pimbackup: unknown show target %q\n", args[0])
		return 2
	}
}

func writeResult(stdout, stderr io.Writer, service *Service, value any, err error) int {
	if err != nil {
		return outputError(stderr, service, err)
	}
	if err := cli.WriteJSON(stdout, value); err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
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
	fmt.Fprintln(stderr, "pimbackup:", service.cleanError(err))
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

type int64List []int64

func (values *int64List) String() string {
	parts := make([]string, len(*values))
	for index, value := range *values {
		parts[index] = strconv.FormatInt(value, 10)
	}
	return strings.Join(parts, ",")
}
func (values *int64List) Set(value string) error {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return errors.New("ID must be a positive integer")
	}
	*values = append(*values, parsed)
	return nil
}
