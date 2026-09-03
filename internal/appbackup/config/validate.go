package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (cfg Config) Validate() error {
	var problems []string
	if cfg.DataDir != "/data" {
		problems = append(problems, "data_dir is fixed at /data")
	}
	if cfg.Restic.Repository != filepath.Join(cfg.DataDir, "restic") {
		problems = append(problems, "restic.repository is fixed at /data/restic")
	}
	if invalidText(cfg.Restic.Binary) {
		problems = append(problems, "restic.binary cannot be empty or contain control characters")
	}
	if cfg.Restic.ResolvedPassword == "" {
		problems = append(problems, "restic.password or restic.password_file is required")
	}
	if cfg.Restic.Timeout.Duration <= 0 {
		problems = append(problems, "restic.timeout must be greater than zero")
	}
	if err := validateServer(cfg.Server); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.Schedule.Interval.Duration < 0 || cfg.Schedule.Enabled && cfg.Schedule.Interval.Duration < time.Minute {
		problems = append(problems, "schedule.interval must be at least 1m when scheduling is enabled")
	}
	if !oneOf(cfg.Log.Level, "debug", "info", "warn", "error") {
		problems = append(problems, "log.level must be debug, info, warn, or error")
	}
	if !oneOf(cfg.Log.Format, "json", "text") {
		problems = append(problems, "log.format must be json or text")
	}
	if cfg.Engine != nil {
		if !oneOf(cfg.Engine.Type, "docker", "podman") || invalidText(cfg.Engine.Binary) || !filepath.IsAbs(cfg.Engine.Socket) || filepath.Clean(cfg.Engine.Socket) != cfg.Engine.Socket || strings.ContainsAny(cfg.Engine.Socket, "\r\n\x00") {
			problems = append(problems, "engine requires type docker or podman, a binary, and an absolute socket path")
		}
	}
	if len(cfg.Applications) > 1000 {
		problems = append(problems, "applications accepts at most 1000 entries")
	}
	seen := map[string]bool{}
	enabled := 0
	for index, app := range cfg.Applications {
		prefix := fmt.Sprintf("applications[%d]", index)
		if app.ID != "" {
			prefix = "application " + strconv.Quote(app.ID)
		}
		if err := validateApplication(cfg.DataDir, app); err != nil {
			problems = append(problems, prefix+": "+err.Error())
		}
		for _, database := range app.Databases {
			if database.VerifyCommand == nil {
				continue
			}
			if engineType := containerEngineType(database.VerifyCommand.Binary); engineType != "" && (cfg.Engine == nil || cfg.Engine.Type != engineType) {
				problems = append(problems, prefix+": database "+strconv.Quote(database.ID)+" container verification requires a matching engine configuration")
			}
		}
		if seen[app.ID] {
			problems = append(problems, prefix+": duplicate application ID")
		}
		seen[app.ID] = true
		if !app.Disabled {
			enabled++
		}
	}
	if cfg.Schedule.Enabled && enabled == 0 {
		problems = append(problems, "schedule.enabled requires at least one enabled application")
	}
	return joinProblems(problems)
}

func validateServer(server ServerConfig) error {
	var problems []string
	host, _, err := net.SplitHostPort(server.Listen)
	if server.Listen == "" || err != nil {
		problems = append(problems, "server.listen must be a host:port address")
	} else if !loopback(host) && server.ResolvedAuthToken == "" && !server.AllowUnauthenticated {
		problems = append(problems, "a non-loopback server.listen requires server.auth_token or server.allow_unauthenticated = true")
	}
	if strings.IndexFunc(server.ResolvedAuthToken, unicode.IsSpace) >= 0 {
		problems = append(problems, "server.auth_token cannot contain whitespace")
	}
	if server.ReadHeaderTimeout.Duration <= 0 || server.ReadTimeout.Duration <= 0 || server.IdleTimeout.Duration <= 0 || server.ShutdownTimeout.Duration <= 0 {
		problems = append(problems, "server timeouts must be greater than zero")
	}
	return joinProblems(problems)
}

func validateApplication(dataDir string, app ApplicationConfig) error {
	var problems []string
	if !idPattern.MatchString(app.ID) {
		problems = append(problems, "id must match "+idPattern.String())
	}
	if app.Timeout.Duration <= 0 {
		problems = append(problems, "timeout must be greater than zero")
	}
	if len(app.Paths) == 0 && len(app.Databases) == 0 {
		problems = append(problems, "at least one path or database is required")
	}
	if len(app.Paths) > 10000 {
		problems = append(problems, "paths accepts at most 10000 entries")
	}
	if len(app.Databases) > 100 {
		problems = append(problems, "databases accepts at most 100 entries")
	}
	paths := map[string]bool{}
	for _, path := range app.Paths {
		if paths[path] {
			problems = append(problems, "paths cannot contain duplicates")
		}
		paths[path] = true
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || invalidText(path) {
			problems = append(problems, "paths must be clean absolute paths")
		}
		if within(dataDir, path) || within(path, dataDir) {
			problems = append(problems, "paths cannot overlap appbackup data beneath /data")
		}
	}
	databaseIDs := map[string]bool{}
	for _, database := range app.Databases {
		if err := validateDatabase(database); err != nil {
			problems = append(problems, "database "+strconv.Quote(database.ID)+": "+err.Error())
		}
		if databaseIDs[database.ID] {
			problems = append(problems, "duplicate database ID "+strconv.Quote(database.ID))
		}
		databaseIDs[database.ID] = true
	}
	for _, phase := range app.Hooks.Phases() {
		if len(phase.Commands) > 100 {
			problems = append(problems, phase.Name+" accepts at most 100 commands")
		}
		for _, command := range phase.Commands {
			if err := validateCommand(command); err != nil {
				problems = append(problems, phase.Name+": "+err.Error())
			}
		}
	}
	return joinProblems(problems)
}

func validateDatabase(database DatabaseConfig) error {
	var problems []string
	if !idPattern.MatchString(database.ID) {
		problems = append(problems, "id must match "+idPattern.String())
	}
	if !oneOf(database.Type, "postgresql", "mysql", "mariadb", "sqlite") {
		problems = append(problems, "type must be postgresql, mysql, mariadb, or sqlite")
	}
	if invalidText(database.Binary) || invalidText(database.RestoreBinary) {
		problems = append(problems, "binary and restore_binary are required")
	}
	if database.Timeout.Duration <= 0 {
		problems = append(problems, "timeout must be greater than zero")
	}
	if database.Type == "sqlite" {
		if !filepath.IsAbs(database.Path) || filepath.Clean(database.Path) != database.Path || invalidText(database.Path) {
			problems = append(problems, "path must be a clean absolute path")
		} else if within("/data", database.Path) || within(database.Path, "/data") {
			problems = append(problems, "path cannot overlap appbackup data beneath /data")
		}
		if database.Host != "" || database.Port != 0 || database.User != "" || database.Name != "" || database.Password != nil {
			problems = append(problems, "sqlite does not accept network connection fields")
		}
	} else {
		if database.Name == "" || strings.HasPrefix(database.Name, "-") || invalidText(database.Name) || strings.ContainsAny(database.Host+database.User, "\r\n\x00") {
			problems = append(problems, "host, user, and name cannot contain control characters, and name must not be empty or start with a dash")
		}
		if database.Port < 0 || database.Port > 65535 {
			problems = append(problems, "port must be between 0 and 65535")
		}
	}
	if database.VerifyCommand != nil {
		if err := validateCommand(*database.VerifyCommand); err != nil {
			problems = append(problems, "verify_command: "+err.Error())
		}
		found := false
		for _, arg := range database.VerifyCommand.Args {
			if strings.Contains(arg, "{dump}") {
				found = true
			}
		}
		if !found {
			problems = append(problems, "verify_command args must contain {dump}")
		}
	}
	return joinProblems(problems)
}

func validateCommand(command CommandConfig) error {
	if invalidText(command.Binary) {
		return errors.New("binary is required and cannot contain control characters")
	}
	if command.Timeout.Duration <= 0 {
		return errors.New("timeout must be greater than zero")
	}
	for _, arg := range command.Args {
		if strings.ContainsAny(arg, "\r\n\x00") {
			return errors.New("arguments cannot contain control characters")
		}
	}
	return nil
}

func invalidText(value string) bool { return value == "" || strings.ContainsAny(value, "\r\n\x00") }
func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
func within(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
func joinProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}
