package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/lauritsk/backup/internal/appbackup"
	"github.com/lauritsk/backup/internal/buildinfo"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(appbackup.RunContext(ctx, os.Args[1:], os.Stdout, os.Stderr, buildinfo.Current()))
}
