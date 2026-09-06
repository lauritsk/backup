// Package cli contains the small command-line mechanics shared by the tools.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Globals are scalar options accepted before or after a command.
type Globals struct {
	ConfigPath *string
	DataDir    *string
	LogLevel   *string
	LogFormat  *string
	JSON       bool
}

// ExtractGlobals removes known global options from args.
func ExtractGlobals(args []string) (Globals, []string, error) {
	var globals Globals
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--json" {
			globals.JSON = true
			continue
		}
		if strings.HasPrefix(argument, "--json=") {
			value := strings.TrimPrefix(argument, "--json=")
			switch value {
			case "true":
				globals.JSON = true
			case "false":
				globals.JSON = false
			default:
				return Globals{}, nil, errors.New("--json must be true or false")
			}
			continue
		}
		name, inline, hasInline := strings.Cut(argument, "=")
		target := globalTarget(&globals, name)
		if target == nil {
			remaining = append(remaining, argument)
			continue
		}
		value := inline
		if !hasInline {
			index++
			if index >= len(args) {
				return Globals{}, nil, fmt.Errorf("%s requires a value", name)
			}
			value = args[index]
		}
		if value == "" {
			return Globals{}, nil, fmt.Errorf("%s cannot be empty", name)
		}
		copy := value
		*target = &copy
	}
	return globals, remaining, nil
}

func globalTarget(globals *Globals, name string) **string {
	switch name {
	case "--config":
		return &globals.ConfigPath
	case "--data-dir":
		return &globals.DataDir
	case "--log-level":
		return &globals.LogLevel
	case "--log-format":
		return &globals.LogFormat
	default:
		return nil
	}
}

// WriteJSON writes indented JSON followed by a newline.
func WriteJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

// WriteNewFile writes a generated configuration to output or to a new 0600
// file. It never replaces an existing file.
func WriteNewFile(output io.Writer, filename string, contents []byte) error {
	if filename == "" || filename == "-" {
		_, err := output.Write(contents)
		return err
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create configuration: %w", err)
	}
	written, writeErr := file.Write(contents)
	if writeErr == nil && written != len(contents) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}
