// Package pimbackup is the composition root for the PIM Backup binary.
package pimbackup

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/pimbackup/config"
	"github.com/lauritsk/backup/internal/pimbackup/model"
)

const usage = `Usage:
  pimbackup [global options] <command> [options]

Commands:
  serve             Run the HTTP API and interval scheduler
  backup            Back up configured PIM accounts
  browse            Browse backed-up mail, contacts, and calendars
  verify            Verify canonical PIM objects and catalog records
  restore           Restore selected PIM objects
  config validate   Validate configuration without network access
  config show       Print effective configuration with secrets redacted
  check             Run storage, SQLite, and IMAP diagnostics
  db rebuild        Rebuild the catalog from canonical files
  version           Print build information
  help              Print this help

Global options:
  --config PATH      JSON file, default /etc/pimbackup/config.json
  --listen ADDRESS   Override server.listen
  --log-level LEVEL  Override log.level
  --log-format TYPE  Override log.format

Global options must appear before the command.
`

// Run executes the root command and returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	global := flag.NewFlagSet("pimbackup", flag.ContinueOnError)
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
	if command == "help" {
		_, _ = io.WriteString(stdout, usage)
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
	switch command {
	case "serve", "backup", "browse", "verify", "restore", "config", "check", "db":
	default:
		fmt.Fprintf(stderr, "pimbackup: unknown command %q\n", command)
		return 2
	}

	overrides := config.Overrides{
		ConfigPath: configPath.pointer(),
		Listen:     listen.pointer(),
		LogLevel:   logLevel.pointer(),
		LogFormat:  logFormat.pointer(),
	}
	cfg, err := config.Load(overrides)
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
	logger, err := newLogger(cfg.Log, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
		return 2
	}

	service, err := OpenService(ctx, cfg, ServiceOptions{Logger: logger})
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
		return 1
	}
	defer service.Close()

	switch command {
	case "serve":
		if len(commandArgs) != 0 {
			fmt.Fprintln(stderr, "pimbackup: serve does not accept arguments")
			return 2
		}
		if err := service.Serve(ctx, info); err != nil {
			fmt.Fprintln(stderr, "pimbackup: serve:", err)
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
			fmt.Fprintln(stderr, "pimbackup: check does not accept arguments")
			return 2
		}
		report := service.Check(ctx)
		writeCLIJSON(stdout, report)
		if report.Status != "ok" {
			return 1
		}
		return 0
	case "db":
		return runDB(ctx, service, commandArgs, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "pimbackup: unknown command %q\n", command)
		return 2
	}
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
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(cfg.RedactedCopy()); err != nil {
			fmt.Fprintln(stderr, "pimbackup: encode configuration:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "pimbackup: unknown config command %q\n", args[0])
		return 2
	}
}

func runBackup(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlags("backup", stderr)
	var accounts stringList
	flags.Var(&accounts, "account", "account ID, repeatable")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "pimbackup: backup accepts flags only")
		return 2
	}
	run, err := service.Backup(ctx, model.BackupRequest{Accounts: accounts})
	writeCLIJSON(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup: backup:", err)
		return 1
	}
	return 0
}

func runVerify(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlags("verify", stderr)
	account := flags.String("account", "", "account ID")
	messageID := flags.Int64("message-id", 0, "single IMAP message ID")
	objectID := flags.Int64("object-id", 0, "single JMAP, contact, or calendar object ID")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *messageID < 0 || *objectID < 0 || *messageID != 0 && *objectID != 0 {
		fmt.Fprintln(stderr, "pimbackup: verify accepts --account and one of --message-id or --object-id")
		return 2
	}
	run, err := service.Verify(ctx, model.VerifyRequest{AccountID: *account, MessageID: *messageID, ObjectID: *objectID})
	writeCLIJSON(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup: verify:", err)
		return 1
	}
	return 0
}

func runRestore(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlags("restore", stderr)
	var messageIDs, objectIDs int64List
	flags.Var(&messageIDs, "message-id", "IMAP message ID, repeatable")
	flags.Var(&objectIDs, "object-id", "JMAP, contact, or calendar object ID, repeatable")
	targetAccount := flags.String("target-account", "", "configured destination account")
	targetMailbox := flags.String("target-mailbox", "", "destination IMAP or JMAP mailbox")
	targetCollection := flags.String("target-collection", "", "destination CardDAV or CalDAV collection")
	createMailbox := flags.Bool("create-mailbox", false, "create the destination mailbox when missing")
	confirm := flags.Bool("yes", false, "confirm the non-idempotent restore")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "pimbackup: restore accepts flags only")
		return 2
	}
	run, err := service.Restore(ctx, model.RestoreRequest{
		MessageIDs: messageIDs, ObjectIDs: objectIDs, TargetAccount: *targetAccount,
		TargetMailbox: *targetMailbox, TargetCollection: *targetCollection,
		CreateMailbox: *createMailbox, Confirm: *confirm,
	})
	writeCLIJSON(stdout, run)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup: restore:", err)
		return 1
	}
	return 0
}

func runBrowse(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: pimbackup browse <accounts|mailboxes|messages|message|collections|objects|object> [options]")
		return 2
	}
	switch args[0] {
	case "accounts":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "pimbackup: browse accounts does not accept arguments")
			return 2
		}
		accounts, err := service.ListAccounts(ctx)
		return outputResult(stdout, stderr, accounts, err)
	case "mailboxes":
		flags := newCommandFlags("browse mailboxes", stderr)
		account := flags.String("account", "", "account ID")
		includeInactive := flags.Bool("include-inactive", false, "include old UIDVALIDITY generations")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return 2
		}
		mailboxes, err := service.ListMailboxes(ctx, *account, *includeInactive)
		return outputResult(stdout, stderr, mailboxes, err)
	case "messages":
		flags := newCommandFlags("browse messages", stderr)
		account := flags.String("account", "", "account ID")
		mailbox := flags.String("mailbox", "", "mailbox name")
		uidValidity := flags.Uint("uid-validity", 0, "UIDVALIDITY generation")
		limit := flags.Int("limit", 100, "maximum rows")
		offset := flags.Int("offset", 0, "rows to skip")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return 2
		}
		if *uidValidity > uint(^uint32(0)) || *limit < 1 || *offset < 0 {
			fmt.Fprintln(stderr, "pimbackup: invalid messages pagination or UIDVALIDITY")
			return 2
		}
		messages, err := service.ListMessages(ctx, model.MessageFilter{
			AccountID:   *account,
			Mailbox:     *mailbox,
			UIDValidity: uint32(*uidValidity),
			Limit:       *limit,
			Offset:      *offset,
		})
		return outputResult(stdout, stderr, messages, err)
	case "collections":
		flags := newCommandFlags("browse collections", stderr)
		account := flags.String("account", "", "account ID")
		kind := flags.String("kind", "", "mail, contact, or calendar")
		includeInactive := flags.Bool("include-inactive", false, "include inactive collections")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return 2
		}
		collections, err := service.ListCollections(ctx, *account, *kind, *includeInactive)
		return outputResult(stdout, stderr, collections, err)
	case "objects":
		flags := newCommandFlags("browse objects", stderr)
		account := flags.String("account", "", "account ID")
		collection := flags.String("collection", "", "collection name")
		kind := flags.String("kind", "", "mail, contact, or calendar")
		limit := flags.Int("limit", 100, "maximum rows")
		offset := flags.Int("offset", 0, "rows to skip")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *limit < 1 || *limit > 1000 || *offset < 0 {
			return 2
		}
		objects, err := service.ListObjects(ctx, model.ObjectFilter{AccountID: *account, Collection: *collection, Kind: *kind, Limit: *limit, Offset: *offset})
		return outputResult(stdout, stderr, objects, err)
	case "object":
		flags := newCommandFlags("browse object", stderr)
		id := flags.Int64("id", 0, "object ID")
		raw := flags.Bool("raw", false, "write the original object")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *id <= 0 {
			fmt.Fprintln(stderr, "usage: pimbackup browse object --id ID [--raw]")
			return 2
		}
		if !*raw {
			object, err := service.GetObject(ctx, *id)
			return outputResult(stdout, stderr, object, err)
		}
		_, file, err := service.OpenObject(ctx, *id)
		if err != nil {
			fmt.Fprintln(stderr, "pimbackup: browse object:", err)
			return 1
		}
		defer file.Close()
		if _, err := io.Copy(stdout, file); err != nil {
			fmt.Fprintln(stderr, "pimbackup: browse object:", err)
			return 1
		}
		return 0
	case "message":
		flags := newCommandFlags("browse message", stderr)
		id := flags.Int64("id", 0, "message ID")
		raw := flags.Bool("raw", false, "write the original RFC822 message")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *id <= 0 {
			fmt.Fprintln(stderr, "usage: pimbackup browse message --id ID [--raw]")
			return 2
		}
		if !*raw {
			message, err := service.GetMessage(ctx, *id)
			return outputResult(stdout, stderr, message, err)
		}
		_, file, err := service.OpenMessage(ctx, *id)
		if err != nil {
			fmt.Fprintln(stderr, "pimbackup: browse message:", err)
			return 1
		}
		defer file.Close()
		if _, err := io.Copy(stdout, file); err != nil {
			fmt.Fprintln(stderr, "pimbackup: browse message:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "pimbackup: unknown browse command %q\n", args[0])
		return 2
	}
}

func runDB(ctx context.Context, service *Service, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "rebuild" {
		fmt.Fprintln(stderr, "usage: pimbackup db rebuild --yes")
		return 2
	}
	flags := newCommandFlags("db rebuild", stderr)
	confirm := flags.Bool("yes", false, "confirm catalog replacement")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return 2
	}
	if !*confirm {
		fmt.Fprintln(stderr, "pimbackup: db rebuild requires --yes")
		return 2
	}
	report, err := service.Rebuild(ctx)
	writeCLIJSON(stdout, report)
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup: db rebuild:", err)
		return 1
	}
	return 0
}

func outputResult(stdout, stderr io.Writer, value any, err error) int {
	if err != nil {
		fmt.Fprintln(stderr, "pimbackup:", err)
		return 1
	}
	writeCLIJSON(stdout, value)
	return 0
}

func writeCLIJSON(output io.Writer, value any) {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func newCommandFlags(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

type optionalString struct {
	value string
	set   bool
}

func (value *optionalString) String() string { return value.value }
func (value *optionalString) Set(input string) error {
	value.value = input
	value.set = true
	return nil
}
func (value *optionalString) pointer() *string {
	if !value.set {
		return nil
	}
	return &value.value
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(input string) error {
	if strings.TrimSpace(input) == "" {
		return errors.New("value cannot be empty")
	}
	*values = append(*values, input)
	return nil
}

type int64List []int64

func (values *int64List) String() string {
	parts := make([]string, len(*values))
	for i, value := range *values {
		parts[i] = strconv.FormatInt(value, 10)
	}
	return strings.Join(parts, ",")
}
func (values *int64List) Set(input string) error {
	parsed, err := strconv.ParseInt(input, 10, 64)
	if err != nil || parsed <= 0 {
		return errors.New("ID must be a positive integer")
	}
	*values = append(*values, parsed)
	return nil
}
