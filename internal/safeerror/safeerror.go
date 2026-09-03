// Package safeerror prepares errors for persistent records and API responses.
package safeerror

import (
	"errors"
	"strings"
)

const maxLength = 2000

// Clean removes line breaks and bounds the error text.
func Clean(err error) error {
	if err == nil {
		return nil
	}
	value := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	if len(value) > maxLength {
		value = value[:maxLength] + "..."
	}
	return errors.New(value)
}
