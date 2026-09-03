package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lauritsk/backup/internal/configutil"
	"github.com/lauritsk/backup/internal/secret"
)

func Load(overrides Overrides) (Config, error) {
	cfg := defaults()
	filename, explicit := selectPath(overrides.ConfigPath)
	if err := configutil.DecodeFile(filename, !explicit, 4<<20, &cfg); err != nil {
		return Config{}, err
	}
	cfg.SourcePath = filename
	normalize(&cfg)
	if err := applyEnvironment(&cfg); err != nil {
		return Config{}, err
	}
	if overrides.Listen != nil {
		cfg.Server.Listen = *overrides.Listen
	}
	if overrides.LogLevel != nil {
		cfg.Log.Level = *overrides.LogLevel
	}
	if overrides.LogFormat != nil {
		cfg.Log.Format = *overrides.LogFormat
	}
	if err := resolveSecrets(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaults() Config {
	return Config{
		DataDir:  "/data",
		Restic:   ResticConfig{Binary: "restic", Repository: "/data/restic", Timeout: duration(4 * time.Hour)},
		Server:   ServerConfig{Listen: "127.0.0.1:8080", ReadHeaderTimeout: duration(5 * time.Second), ReadTimeout: duration(30 * time.Second), IdleTimeout: duration(time.Minute), ShutdownTimeout: duration(15 * time.Second)},
		Schedule: ScheduleConfig{Interval: duration(24 * time.Hour)},
		Log:      LogConfig{Level: "info", Format: "json"},
	}
}

func duration(value time.Duration) Duration { return Duration{Duration: value} }

func selectPath(value *string) (string, bool) {
	if value != nil {
		return *value, true
	}
	if value, ok := os.LookupEnv("APPBACKUP_CONFIG"); ok {
		return value, true
	}
	return DefaultPath, false
}

func normalize(cfg *Config) {
	if cfg.Restic.Binary == "" {
		cfg.Restic.Binary = "restic"
	}
	if cfg.Restic.Repository == "" {
		cfg.Restic.Repository = filepath.Join(cfg.DataDir, "restic")
	}
	for i := range cfg.Applications {
		app := &cfg.Applications[i]
		if app.Timeout.Duration == 0 {
			app.Timeout = duration(4 * time.Hour)
		}
		for j := range app.Databases {
			database := &app.Databases[j]
			if database.Timeout.Duration == 0 {
				database.Timeout = duration(time.Hour)
			}
			if database.Binary == "" {
				switch database.Type {
				case "postgresql":
					database.Binary = "pg_dump"
				case "mysql", "mariadb":
					database.Binary = "mysqldump"
				case "sqlite":
					database.Binary = "sqlite3"
				}
			}
			if database.RestoreBinary == "" {
				switch database.Type {
				case "postgresql":
					database.RestoreBinary = "pg_restore"
				case "mysql", "mariadb":
					database.RestoreBinary = "mysql"
				case "sqlite":
					database.RestoreBinary = "sqlite3"
				}
			}
			normalizeCommand(database.VerifyCommand)
		}
		for _, phase := range app.Hooks.Phases() {
			for j := range phase.Commands {
				if phase.Commands[j].Timeout.Duration == 0 {
					phase.Commands[j].Timeout = duration(5 * time.Minute)
				}
			}
		}
	}
	if cfg.Engine != nil && cfg.Engine.Binary == "" {
		cfg.Engine.Binary = cfg.Engine.Type
	}
}

func normalizeCommand(command *CommandConfig) {
	if command != nil && command.Timeout.Duration == 0 {
		command.Timeout = duration(30 * time.Minute)
	}
}

func applyEnvironment(cfg *Config) error {
	values := []struct {
		name   string
		target *string
	}{
		{"APPBACKUP_LISTEN", &cfg.Server.Listen}, {"APPBACKUP_LOG_LEVEL", &cfg.Log.Level}, {"APPBACKUP_LOG_FORMAT", &cfg.Log.Format},
		{"APPBACKUP_RESTIC_BINARY", &cfg.Restic.Binary}, {"APPBACKUP_RESTIC_REPOSITORY", &cfg.Restic.Repository},
	}
	for _, item := range values {
		if value, ok := os.LookupEnv(item.name); ok {
			*item.target = value
		}
	}
	for _, item := range []struct {
		name   string
		target *bool
	}{
		{"APPBACKUP_SCHEDULE_ENABLED", &cfg.Schedule.Enabled}, {"APPBACKUP_SCHEDULE_RUN_ON_START", &cfg.Schedule.RunOnStart}, {"APPBACKUP_ALLOW_UNAUTHENTICATED", &cfg.Server.AllowUnauthenticated},
	} {
		if err := configutil.EnvBool(item.name, item.target); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		name   string
		target *Duration
	}{
		{"APPBACKUP_SCHEDULE_INTERVAL", &cfg.Schedule.Interval},
		{"APPBACKUP_RESTIC_TIMEOUT", &cfg.Restic.Timeout},
	} {
		if value, ok := os.LookupEnv(item.name); ok {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("%s: %w", item.name, err)
			}
			item.target.Duration = parsed
		}
	}
	return nil
}

func resolveSecrets(cfg *Config) error {
	password, set, err := resolveSecret("APPBACKUP_RESTIC_PASSWORD", "restic.password", cfg.Restic.Password, cfg.Restic.PasswordFile)
	if err != nil {
		return err
	}
	if set {
		cfg.Restic.ResolvedPassword = password
		cfg.Restic.Password = pointer(password)
		cfg.Restic.PasswordFile = nil
	}
	token, set, err := resolveSecret("APPBACKUP_API_TOKEN", "server.auth_token", cfg.Server.AuthToken, cfg.Server.AuthTokenFile)
	if err != nil {
		return err
	}
	if set {
		cfg.Server.ResolvedAuthToken = token
		cfg.Server.AuthToken = pointer(token)
		cfg.Server.AuthTokenFile = nil
	}
	for appIndex := range cfg.Applications {
		for dbIndex := range cfg.Applications[appIndex].Databases {
			database := &cfg.Applications[appIndex].Databases[dbIndex]
			name := fmt.Sprintf("application %q database %q password", cfg.Applications[appIndex].ID, database.ID)
			value, set, err := secret.Resolve(secret.Source{Name: name, Direct: deref(database.Password), DirectSet: database.Password != nil, File: deref(database.PasswordFile), FileSet: database.PasswordFile != nil})
			if err != nil {
				return err
			}
			if set {
				database.ResolvedPassword = value
				database.Password = pointer(value)
				database.PasswordFile = nil
			}
		}
	}
	return nil
}

func resolveSecret(env, name string, direct, file *string) (string, bool, error) {
	envDirect, directSet := os.LookupEnv(env)
	envFile, fileSet := os.LookupEnv(env + "_FILE")
	if directSet || fileSet {
		return secret.Resolve(secret.Source{Name: env, Direct: envDirect, DirectSet: directSet, File: envFile, FileSet: fileSet})
	}
	return secret.Resolve(secret.Source{Name: name, Direct: deref(direct), DirectSet: direct != nil, File: deref(file), FileSet: file != nil})
}

func pointer(value string) *string { result := value; return &result }
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
