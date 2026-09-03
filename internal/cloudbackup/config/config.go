// Package config loads and validates Cloud Backup configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lauritsk/backup/internal/configutil"
	"github.com/lauritsk/backup/internal/secret"
)

const (
	DefaultPath = "/etc/cloudbackup/config.json"
	Redacted    = "<redacted>"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
var remotePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,126}:`)
var bandwidthPattern = regexp.MustCompile(`^(off|[0-9]+(?:\.[0-9]+)?[bBkKmMgGtTpP]?(?:/[sS])?)$`)

type Duration = configutil.Duration

type Config struct {
	DataDir    string         `json:"data_dir"`
	Rclone     RcloneConfig   `json:"rclone"`
	Server     ServerConfig   `json:"server"`
	Schedule   ScheduleConfig `json:"schedule"`
	Log        LogConfig      `json:"log"`
	Sources    []SourceConfig `json:"sources"`
	SourcePath string         `json:"-"`
}

type RcloneConfig struct {
	ConfigPath string `json:"config_path,omitempty"`
}

type ServerConfig struct {
	Listen               string   `json:"listen"`
	ReadHeaderTimeout    Duration `json:"read_header_timeout"`
	ReadTimeout          Duration `json:"read_timeout"`
	IdleTimeout          Duration `json:"idle_timeout"`
	ShutdownTimeout      Duration `json:"shutdown_timeout"`
	AuthToken            *string  `json:"auth_token,omitempty"`
	AuthTokenFile        *string  `json:"auth_token_file,omitempty"`
	AllowUnauthenticated bool     `json:"allow_unauthenticated"`
	ResolvedAuthToken    string   `json:"-"`
}

type ScheduleConfig struct {
	Enabled    bool     `json:"enabled"`
	Interval   Duration `json:"interval"`
	RunOnStart bool     `json:"run_on_start"`
}

type LogConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

type SourceConfig struct {
	ID             string   `json:"id"`
	Remote         string   `json:"remote"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	BandwidthLimit string   `json:"bandwidth_limit,omitempty"`
	Transfers      int      `json:"transfers,omitempty"`
	Checkers       int      `json:"checkers,omitempty"`
	Timeout        Duration `json:"timeout"`
	Disabled       bool     `json:"disabled"`
}

type Overrides struct{ ConfigPath, Listen, LogLevel, LogFormat *string }

func Load(overrides Overrides) (Config, error) {
	cfg := defaults()
	filename, explicit := selectPath(overrides.ConfigPath)
	if err := decodeFile(filename, explicit, &cfg); err != nil {
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
		DataDir: "/data",
		Server: ServerConfig{
			Listen:            "127.0.0.1:8080",
			ReadHeaderTimeout: Duration{Duration: 5 * time.Second},
			ReadTimeout:       Duration{Duration: 30 * time.Second},
			IdleTimeout:       Duration{Duration: 60 * time.Second},
			ShutdownTimeout:   Duration{Duration: 15 * time.Second},
		},
		Schedule: ScheduleConfig{Interval: Duration{Duration: 24 * time.Hour}},
		Log:      LogConfig{Level: "info", Format: "json"},
	}
}

func selectPath(value *string) (string, bool) {
	if value != nil {
		return *value, true
	}
	if value, ok := os.LookupEnv("CLOUDBACKUP_CONFIG"); ok {
		return value, true
	}
	return DefaultPath, false
}

func decodeFile(filename string, explicit bool, target *Config) error {
	return configutil.DecodeFile(filename, !explicit, 4<<20, target)
}

func normalize(cfg *Config) {
	for index := range cfg.Sources {
		if cfg.Sources[index].Timeout.Duration == 0 {
			cfg.Sources[index].Timeout.Duration = 30 * time.Minute
		}
	}
}

func applyEnvironment(cfg *Config) error {
	values := []struct {
		name   string
		target *string
	}{
		{"CLOUDBACKUP_LISTEN", &cfg.Server.Listen}, {"CLOUDBACKUP_LOG_LEVEL", &cfg.Log.Level}, {"CLOUDBACKUP_LOG_FORMAT", &cfg.Log.Format},
		{"CLOUDBACKUP_RCLONE_CONFIG_PATH", &cfg.Rclone.ConfigPath},
	}
	for _, item := range values {
		if value, ok := os.LookupEnv(item.name); ok {
			*item.target = value
		}
	}
	for _, item := range []struct {
		name   string
		target *bool
	}{{"CLOUDBACKUP_SCHEDULE_ENABLED", &cfg.Schedule.Enabled}, {"CLOUDBACKUP_SCHEDULE_RUN_ON_START", &cfg.Schedule.RunOnStart}, {"CLOUDBACKUP_ALLOW_UNAUTHENTICATED", &cfg.Server.AllowUnauthenticated}} {
		if err := configutil.EnvBool(item.name, item.target); err != nil {
			return err
		}
	}
	if value, ok := os.LookupEnv("CLOUDBACKUP_SCHEDULE_INTERVAL"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("CLOUDBACKUP_SCHEDULE_INTERVAL: %w", err)
		}
		cfg.Schedule.Interval.Duration = parsed
	}
	return nil
}

func resolveSecrets(cfg *Config) error {
	token, set, err := resolveSecret("CLOUDBACKUP_API_TOKEN", "server.auth_token", cfg.Server.AuthToken, cfg.Server.AuthTokenFile)
	if err != nil {
		return err
	}
	if set {
		cfg.Server.ResolvedAuthToken = token
		cfg.Server.AuthToken = pointer(token)
		cfg.Server.AuthTokenFile = nil
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
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func pointer(value string) *string { result := value; return &result }

func (cfg Config) Validate() error {
	var problems []string
	if cfg.DataDir != "/data" {
		problems = append(problems, "data_dir is fixed at /data")
	}
	if strings.ContainsAny(cfg.Rclone.ConfigPath, "\r\n\x00") {
		problems = append(problems, "rclone.config_path cannot contain control characters")
	}
	if err := validateServer(cfg.Server); err != nil {
		problems = append(problems, err.Error())
	}
	if cfg.Schedule.Interval.Duration < 0 {
		problems = append(problems, "schedule.interval cannot be negative")
	}
	if cfg.Schedule.Enabled && cfg.Schedule.Interval.Duration < time.Minute {
		problems = append(problems, "schedule.interval must be at least 1m when scheduling is enabled")
	}
	if cfg.Log.Level != "debug" && cfg.Log.Level != "info" && cfg.Log.Level != "warn" && cfg.Log.Level != "error" {
		problems = append(problems, "log.level must be debug, info, warn, or error")
	}
	if cfg.Log.Format != "json" && cfg.Log.Format != "text" {
		problems = append(problems, "log.format must be json or text")
	}
	seen := map[string]bool{}
	enabled := 0
	for index, source := range cfg.Sources {
		prefix := fmt.Sprintf("sources[%d]", index)
		if source.ID != "" {
			prefix = "source " + strconv.Quote(source.ID)
		}
		if err := validateSource(source); err != nil {
			problems = append(problems, prefix+": "+err.Error())
		}
		if seen[source.ID] {
			problems = append(problems, prefix+": duplicate source ID")
		}
		seen[source.ID] = true
		if !source.Disabled {
			enabled++
		}
	}
	if cfg.Schedule.Enabled && enabled == 0 {
		problems = append(problems, "schedule.enabled requires at least one enabled source")
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
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
	return errors.Join(toErrors(problems)...)
}
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func validateSource(source SourceConfig) error {
	var problems []string
	if !idPattern.MatchString(source.ID) {
		problems = append(problems, "id must match "+idPattern.String())
	}
	if !remotePattern.MatchString(source.Remote) || strings.ContainsAny(source.Remote, "\r\n\x00") || strings.Contains(source.Remote, "://") {
		problems = append(problems, "remote must start with a configured rclone remote name followed by a colon")
	}
	for _, filter := range append(append([]string{}, source.Include...), source.Exclude...) {
		if filter == "" || strings.ContainsAny(filter, "\r\n\x00") {
			problems = append(problems, "include and exclude rules cannot be empty or contain control characters")
		}
	}
	if source.BandwidthLimit != "" && !bandwidthPattern.MatchString(source.BandwidthLimit) {
		problems = append(problems, "bandwidth_limit is not a valid rclone rate")
	}
	if source.Transfers < 0 || source.Transfers > 256 {
		problems = append(problems, "transfers must be between 0 and 256")
	}
	if source.Checkers < 0 || source.Checkers > 256 {
		problems = append(problems, "checkers must be between 0 and 256")
	}
	if source.Timeout.Duration <= 0 {
		problems = append(problems, "timeout must be greater than zero")
	}
	return errors.Join(toErrors(problems)...)
}
func toErrors(values []string) []error {
	result := make([]error, len(values))
	for i := range values {
		result[i] = errors.New(values[i])
	}
	return result
}

func (cfg Config) EnabledSources() []SourceConfig {
	result := []SourceConfig{}
	for _, source := range cfg.Sources {
		if !source.Disabled {
			result = append(result, source)
		}
	}
	return result
}
func (cfg Config) Source(id string) (SourceConfig, bool) {
	for _, source := range cfg.Sources {
		if source.ID == id {
			return source, true
		}
	}
	return SourceConfig{}, false
}
func (cfg Config) RedactedCopy() Config {
	result := cfg
	result.Server = cfg.Server
	result.Rclone = cfg.Rclone
	if cfg.Server.ResolvedAuthToken != "" {
		result.Server.AuthToken = pointer(Redacted)
	} else {
		result.Server.AuthToken = nil
	}
	result.Server.AuthTokenFile = nil
	result.Server.ResolvedAuthToken = ""
	return result
}
