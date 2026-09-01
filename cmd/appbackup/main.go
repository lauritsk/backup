package main

import (
	"os"

	"github.com/lauritsk/backup/internal/appbackup"
	"github.com/lauritsk/backup/internal/buildinfo"
)

func main() {
	os.Exit(appbackup.Run(os.Args[1:], os.Stdout, os.Stderr, buildinfo.Current()))
}
