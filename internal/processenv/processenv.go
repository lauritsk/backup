// Package processenv builds child-process environments without inherited backup secrets.
package processenv

import (
	"os"
	"strings"
)

// Without returns the current environment without the named variables.
func Without(names ...string) []string {
	return without(os.Environ(), nil, names...)
}

// WithoutPrefixes also removes variables whose names start with a prefix.
func WithoutPrefixes(prefixes []string, names ...string) []string {
	return without(os.Environ(), prefixes, names...)
}

func without(environ, prefixes []string, names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[strings.ToUpper(name)] = struct{}{}
	}
	upperPrefixes := make([]string, len(prefixes))
	for index, prefix := range prefixes {
		upperPrefixes[index] = strings.ToUpper(prefix)
	}
	result := make([]string, 0, len(environ))
	for _, item := range environ {
		name, _, _ := strings.Cut(item, "=")
		upperName := strings.ToUpper(name)
		_, remove := blocked[upperName]
		for _, prefix := range upperPrefixes {
			remove = remove || strings.HasPrefix(upperName, prefix)
		}
		if !remove {
			result = append(result, item)
		}
	}
	return result
}
