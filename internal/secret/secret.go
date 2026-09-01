// Package secret implements the direct value and *_FILE secret convention.
package secret

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

// Source preserves whether each setting was present. Empty environment values
// still count as set, which lets Resolve reject ambiguous configuration.
type Source struct {
	Name      string
	Direct    string
	DirectSet bool
	File      string
	FileSet   bool
}

// Resolve returns a configured secret and whether either source was set.
// A secret file may end in one LF or CRLF, which Resolve removes. It preserves
// all other whitespace.
func Resolve(source Source) (value string, set bool, err error) {
	directName, fileName := names(source.Name)

	if source.DirectSet && source.FileSet {
		return "", false, fmt.Errorf("secret: %s and %s cannot both be set", directName, fileName)
	}
	if source.DirectSet {
		return source.Direct, true, nil
	}
	if !source.FileSet {
		return "", false, nil
	}
	if source.File == "" {
		return "", false, fmt.Errorf("secret: %s is set but has an empty path", fileName)
	}

	info, statErr := os.Stat(source.File)
	if statErr != nil {
		return "", false, fmt.Errorf("secret: inspect %s: %w", fileName, statErr)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("secret: %s does not name a regular file", fileName)
	}
	if info.Size() > 1<<20 {
		return "", false, fmt.Errorf("secret: %s exceeds 1 MiB", fileName)
	}
	file, openErr := os.Open(source.File)
	if openErr != nil {
		return "", false, fmt.Errorf("secret: open %s: %w", fileName, openErr)
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	closeErr := file.Close()
	if readErr != nil {
		return "", false, fmt.Errorf("secret: read %s: %w", fileName, readErr)
	}
	if closeErr != nil {
		return "", false, fmt.Errorf("secret: close %s: %w", fileName, closeErr)
	}
	if len(contents) > 1<<20 {
		return "", false, fmt.Errorf("secret: %s exceeds 1 MiB", fileName)
	}
	contents = trimOneLineEnding(contents)
	return string(contents), true, nil
}

func names(name string) (direct string, file string) {
	if name == "" {
		return "direct value", "file value"
	}
	return name, name + "_FILE"
}

func trimOneLineEnding(value []byte) []byte {
	if bytes.HasSuffix(value, []byte("\r\n")) {
		return value[:len(value)-2]
	}
	if bytes.HasSuffix(value, []byte("\n")) {
		return value[:len(value)-1]
	}
	return value
}
