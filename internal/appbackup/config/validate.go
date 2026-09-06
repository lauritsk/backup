package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (cfg Config) Validate() error {
	var problems []string
	if !filepath.IsAbs(cfg.DataDir) || filepath.Clean(cfg.DataDir) != cfg.DataDir || cfg.DataDir == string(filepath.Separator) || strings.ContainsAny(cfg.DataDir, "\r\n\x00") {
		problems = append(problems, "data_dir must be a clean absolute path other than the filesystem root")
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
	if !oneOf(cfg.Log.Level, "debug", "info", "warn", "error") {
		problems = append(problems, "log.level must be debug, info, warn, or error")
	}
	if !oneOf(cfg.Log.Format, "json", "text") {
		problems = append(problems, "log.format must be json or text")
	}
	if len(cfg.Applications) > 1000 {
		problems = append(problems, "applications accepts at most 1000 entries")
	}
	seen := map[string]bool{}
	for index, app := range cfg.Applications {
		prefix := fmt.Sprintf("applications[%d]", index)
		if app.ID != "" {
			prefix = "application " + strconv.Quote(app.ID)
		}
		if err := validateApplication(cfg.DataDir, app); err != nil {
			problems = append(problems, prefix+": "+err.Error())
		}
		if seen[app.ID] {
			problems = append(problems, prefix+": duplicate application ID")
		}
		seen[app.ID] = true
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
			problems = append(problems, "paths cannot overlap appbackup data_dir")
		}
	}
	databaseIDs := map[string]bool{}
	for _, database := range app.Databases {
		if err := validateDatabase(dataDir, database); err != nil {
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

func validateDatabase(dataDir string, database DatabaseConfig) error {
	var problems []string
	if !idPattern.MatchString(database.ID) {
		problems = append(problems, "id must match "+idPattern.String())
	}
	if !oneOf(database.Type, "postgresql", "mysql", "mariadb", "sqlite") {
		problems = append(problems, "type must be postgresql, mysql, mariadb, or sqlite")
	}
	switch database.Type {
	case "postgresql":
		if invalidText(database.Binary) || invalidText(database.RestoreBinary) {
			problems = append(problems, "binary and restore_binary are required")
		}
	case "mysql", "mariadb":
		if invalidText(database.Binary) {
			problems = append(problems, "binary is required")
		}
		if database.RestoreBinary != "" {
			problems = append(problems, "restore_binary is not used for mysql or mariadb")
		}
	case "sqlite":
		if database.Binary != "" || database.RestoreBinary != "" {
			problems = append(problems, "sqlite does not use binary or restore_binary")
		}
	}
	if database.Timeout.Duration <= 0 {
		problems = append(problems, "timeout must be greater than zero")
	}
	if database.Type == "sqlite" {
		if !filepath.IsAbs(database.Path) || filepath.Clean(database.Path) != database.Path || invalidText(database.Path) {
			problems = append(problems, "path must be a clean absolute path")
		} else if within(dataDir, database.Path) || within(database.Path, dataDir) {
			problems = append(problems, "path cannot overlap appbackup data_dir")
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
func joinProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}
