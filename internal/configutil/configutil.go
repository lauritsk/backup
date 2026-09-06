// Package configutil contains configuration mechanics shared by the binaries.
package configutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// Duration is a JSON duration encoded as a string such as "30s" or "24h".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(value []byte) error {
	parsed, err := time.ParseDuration(string(value))
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.Duration.String()), nil }

// DecodeFile reads one bounded JSON value, rejects duplicate and unknown
// fields, and optionally ignores a missing file.
func DecodeFile(filename string, optional bool, maxBytes int64, target any) error {
	if filename == "" {
		return errors.New("config path cannot be empty")
	}
	file, err := os.Open(filename)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load config %q: %w", filename, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("load config %q: inspect file: %w", filename, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("load config %q: path is not a regular file", filename)
	}
	if info.Size() > maxBytes {
		if maxBytes == 4<<20 {
			return fmt.Errorf("load config %q: file exceeds 4 MiB", filename)
		}
		return fmt.Errorf("load config %q: file exceeds %d bytes", filename, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return fmt.Errorf("load config %q: %w", filename, err)
	}
	if int64(len(data)) > maxBytes {
		if maxBytes == 4<<20 {
			return fmt.Errorf("load config %q: file exceeds 4 MiB", filename)
		}
		return fmt.Errorf("load config %q: file exceeds %d bytes", filename, maxBytes)
	}
	if err := DecodeStrict(data, target); err != nil {
		return fmt.Errorf("load config %q: %w", filename, err)
	}
	return nil
}

// DecodeStrict decodes exactly one JSON value and rejects unknown and
// duplicate object fields.
func DecodeStrict(data []byte, target any) error {
	if err := rejectDuplicateNames(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("configuration must contain one JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateNames(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var parseValue func(json.Token) error
	parseValue = func(token json.Token) error {
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := nameToken.(string)
				if !ok {
					return errors.New("JSON object name is not a string")
				}
				if _, duplicate := seen[name]; duplicate {
					return fmt.Errorf("duplicate JSON field %q", name)
				}
				seen[name] = struct{}{}
				value, err := decoder.Token()
				if err != nil {
					return err
				}
				if err := parseValue(value); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				value, err := decoder.Token()
				if err != nil {
					return err
				}
				if err := parseValue(value); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := parseValue(first); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("configuration must contain one JSON value")
		}
		return err
	}
	return nil
}

// EnvBool applies a boolean environment variable when it is present.
func EnvBool(name string, target *bool) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*target = parsed
	return nil
}
