package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/cloudbackup"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(cloudbackup.RunContext(ctx, os.Args[1:], os.Stdout, os.Stderr, buildinfo.Current()))
}
