package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/lauritsk/backup/internal/buildinfo"
	"github.com/lauritsk/backup/internal/pimbackup"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	os.Exit(pimbackup.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, buildinfo.Current()))
}
