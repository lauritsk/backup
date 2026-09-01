// Package logging constructs the suite's structured loggers.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New constructs a JSON or text logger at the requested level.
func New(level, format string, output io.Writer) (*slog.Logger, error) {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("parse log level: %w", err)
	}
	options := &slog.HandlerOptions{Level: parsed}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(output, options)), nil
	case "text":
		return slog.New(slog.NewTextHandler(output, options)), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q", format)
	}
}
