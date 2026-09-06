// Package safeerror removes credentials and unsafe text from persisted errors.
package safeerror

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxLength = 2000

var (
	urlPattern        = regexp.MustCompile(`(?i)https?://[^\s]+`)
	bearerPattern     = regexp.MustCompile(`(?i)\bBearer[ \t]+[^\s,;]+`)
	credentialPattern = regexp.MustCompile(`(?i)\b(password|passwd|token|secret|authorization|credential)([=:])[^\s,;]+`)
)

// Redactor cleans errors and removes exact configured secret values.
type Redactor struct {
	secrets []string
}

// New returns a redactor for non-empty configured secrets.
func New(secrets ...string) Redactor {
	values := make([]string, 0, len(secrets))
	seen := make(map[string]struct{}, len(secrets))
	for _, value := range secrets {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		for _, candidate := range []string{value, url.PathEscape(value), url.QueryEscape(value)} {
			if candidate == "" {
				continue
			}
			if _, exists := seen[candidate]; exists {
				continue
			}
			seen[candidate] = struct{}{}
			values = append(values, candidate)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return Redactor{secrets: values}
}

// Clean removes likely credentials, URL user information and queries, control
// characters, and excess text.
func Clean(err error) error {
	return Redactor{}.Clean(err)
}

// Clean applies the redactor to err.
func (r Redactor) Clean(err error) error {
	if err == nil {
		return nil
	}
	value := err.Error()
	for _, secret := range r.secrets {
		value = strings.ReplaceAll(value, secret, "<redacted>")
	}
	value = bearerPattern.ReplaceAllString(value, "Bearer <redacted>")
	value = credentialPattern.ReplaceAllString(value, "$1$2<redacted>")
	value = urlPattern.ReplaceAllStringFunc(value, cleanURL)
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, value)
	if utf8.RuneCountInString(value) > maxLength {
		runes := []rune(value)
		value = string(runes[:maxLength]) + "..."
	}
	return errors.New(value)
}

func cleanURL(value string) string {
	trailing := ""
	for len(value) > 0 && strings.ContainsRune(`.,;:!?)\]}`, rune(value[len(value)-1])) {
		trailing = value[len(value)-1:] + trailing
		value = value[:len(value)-1]
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "<redacted-url>" + trailing
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String() + trailing
}
