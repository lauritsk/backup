package pimbackup

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/lauritsk/backup/internal/pimbackup/config"
)

func newLogger(cfg config.LogConfig, output io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(cfg.Level))); err != nil {
		return nil, fmt.Errorf("parse log level: %w", err)
	}
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch cfg.Format {
	case "json":
		handler = slog.NewJSONHandler(output, options)
	case "text":
		handler = slog.NewTextHandler(output, options)
	default:
		return nil, fmt.Errorf("unsupported log format %q", cfg.Format)
	}
	return slog.New(handler), nil
}
