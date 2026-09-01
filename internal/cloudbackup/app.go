// Package cloudbackup is the composition root for the Cloud Backup binary.
// Source acquisition and recovery behavior in this package must remain
// independent of PIM Backup and Application Backup.
package cloudbackup

import (
	"fmt"
	"io"

	"github.com/lauritsk/backup/internal/buildinfo"
)

const usage = `Usage:
  cloudbackup <command> [options]

Commands:
  serve             Run the HTTP API and interval scheduler
  backup            Acquire configured sources with rclone
  browse            Read acquired files and recovery metadata
  verify            Verify acquired files and manifests
  restore           Export selected data beneath /data
  config validate   Validate configuration without network access
  config show       Print effective configuration with secrets redacted
  check             Run storage, rclone, and source diagnostics
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
			fmt.Fprintln(stderr, "cloudbackup: version does not accept arguments")
			return 2
		}
		fmt.Fprintln(stdout, info.Format("cloudbackup"))
		return 0
	case "serve", "backup", "browse", "verify", "restore", "config", "check":
		fmt.Fprintf(stderr, "cloudbackup: command %q is not implemented yet\n", args[0])
		return 1
	default:
		fmt.Fprintf(stderr, "cloudbackup: unknown command %q\n", args[0])
		return 2
	}
}
