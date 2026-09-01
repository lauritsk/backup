// Package appbackup is the composition root for the Application Backup
// binary. Recovery-point planning, native dump tools, Restic, hooks, and
// optional container engine access belong here.
package appbackup

import (
	"fmt"
	"io"

	"github.com/lauritsk/backup/internal/buildinfo"
)

const usage = `Usage:
  appbackup <command> [options]

Commands:
  serve             Run the HTTP API and interval scheduler
  backup            Create an application recovery point
  browse            Read recovery points and their contents
  verify            Test recovery-point integrity and database dumps
  restore           Restore a selected application recovery point
  config validate   Validate configuration without network access
  config show       Print effective configuration with secrets redacted
  check             Run storage, tool, database, and container diagnostics
  version           Print build information
  help              Print this help

This scaffold currently implements help and version only.
`

// Run executes the root command and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}

	switch args[0] {
	case "help", "-h", "--help":
		_, _ = io.WriteString(stdout, usage)
		return 0
	case "version":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "appbackup: version does not accept arguments")
			return 2
		}
		fmt.Fprintln(stdout, info.Format("appbackup"))
		return 0
	case "serve", "backup", "browse", "verify", "restore", "config", "check":
		fmt.Fprintf(stderr, "appbackup: command %q is not implemented yet\n", args[0])
		return 1
	default:
		fmt.Fprintf(stderr, "appbackup: unknown command %q\n", args[0])
		return 2
	}
}
