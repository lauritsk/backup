// Package buildinfo reports metadata embedded in a backup suite binary.
package buildinfo

import (
	"fmt"
	"runtime"
)

// These values can be replaced with go build -ldflags.
var (
	Version   = "dev"
	Revision  = "unknown"
	BuildTime = "unknown"
)

// Info is safe to return from version commands and HTTP endpoints.
type Info struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
}

// Current returns the metadata for the running binary.
func Current() Info {
	return Info{
		Version:   valueOr(Version, "dev"),
		Revision:  valueOr(Revision, "unknown"),
		BuildTime: valueOr(BuildTime, "unknown"),
		GoVersion: runtime.Version(),
	}
}

// Format returns a stable, single-line representation for a version command.
func (i Info) Format(program string) string {
	return fmt.Sprintf(
		"%s version=%s revision=%s build_time=%s go=%s",
		program,
		valueOr(i.Version, "dev"),
		valueOr(i.Revision, "unknown"),
		valueOr(i.BuildTime, "unknown"),
		valueOr(i.GoVersion, "unknown"),
	)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
