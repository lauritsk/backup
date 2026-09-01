package main

import (
	"os"

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/cloudbackup"
)

func main() {
	os.Exit(cloudbackup.Run(os.Args[1:], os.Stdout, os.Stderr, buildinfo.Current()))
}
