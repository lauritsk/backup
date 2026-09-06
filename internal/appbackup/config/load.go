package config

import (
	"fmt"
	"os"
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
	if overrides.DataDir != nil {
		cfg.DataDir = *overrides.DataDir
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
		DataDir: "/data",
		Restic:  ResticConfig{Binary: "restic", Timeout: duration(4 * time.Hour)},
		Log:     LogConfig{Level: "info", Format: "text"},
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
				case "mysql":
					database.Binary = "mysqldump"
				case "mariadb":
					database.Binary = "mariadb-dump"
				}
			}
			if database.Type == "postgresql" && database.RestoreBinary == "" {
				database.RestoreBinary = "pg_restore"
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
		{"APPBACKUP_DATA_DIR", &cfg.DataDir}, {"APPBACKUP_LOG_LEVEL", &cfg.Log.Level}, {"APPBACKUP_LOG_FORMAT", &cfg.Log.Format},
		{"APPBACKUP_RESTIC_BINARY", &cfg.Restic.Binary},
	}
	for _, item := range values {
		if value, ok := os.LookupEnv(item.name); ok {
			*item.target = value
		}
	}
	for _, item := range []struct {
		name   string
		target *Duration
	}{
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
	password, passwordFile, set, err := resolveSecret("APPBACKUP_RESTIC_PASSWORD", "restic.password", cfg.Restic.Password, cfg.Restic.PasswordFile)
	if err != nil {
		return err
	}
	if set {
		cfg.Restic.ResolvedPassword = password
		cfg.Restic.ResolvedPasswordFile = passwordFile
		cfg.Restic.Password = pointer(password)
	}
	for appIndex := range cfg.Applications {
		for dbIndex := range cfg.Applications[appIndex].Databases {
			database := &cfg.Applications[appIndex].Databases[dbIndex]
			name := fmt.Sprintf("application %q database %q password", cfg.Applications[appIndex].ID, database.ID)
			configuredFile := deref(database.PasswordFile)
			value, set, err := secret.Resolve(secret.Source{Name: name, Direct: deref(database.Password), DirectSet: database.Password != nil, File: configuredFile, FileSet: database.PasswordFile != nil})
			if err != nil {
				return err
			}
			if set {
				database.ResolvedPassword = value
				if database.Password == nil {
					database.ResolvedPasswordFile = configuredFile
				}
				database.Password = pointer(value)
			}
		}
	}
	return nil
}

func resolveSecret(env, name string, direct, file *string) (value, filePath string, set bool, err error) {
	envDirect, directSet := os.LookupEnv(env)
	envFile, fileSet := os.LookupEnv(env + "_FILE")
	if directSet || fileSet {
		value, set, err = secret.Resolve(secret.Source{Name: env, Direct: envDirect, DirectSet: directSet, File: envFile, FileSet: fileSet})
		if fileSet && !directSet {
			filePath = envFile
		}
		return
	}
	value, set, err = secret.Resolve(secret.Source{Name: name, Direct: deref(direct), DirectSet: direct != nil, File: deref(file), FileSet: file != nil})
	if file != nil && direct == nil {
		filePath = *file
	}
	return
}

func pointer(value string) *string { result := value; return &result }
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
