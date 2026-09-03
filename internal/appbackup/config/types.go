package config

import "github.com/lauritsk/backup/internal/configutil"

const (
	DefaultPath = "/etc/appbackup/config.json"
	Redacted    = "<redacted>"
)

type Duration = configutil.Duration

type Config struct {
	DataDir      string              `json:"data_dir"`
	Restic       ResticConfig        `json:"restic"`
	Server       ServerConfig        `json:"server"`
	Schedule     ScheduleConfig      `json:"schedule"`
	Log          LogConfig           `json:"log"`
	Engine       *EngineConfig       `json:"engine,omitempty"`
	Applications []ApplicationConfig `json:"applications"`
	SourcePath   string              `json:"-"`
}

type ResticConfig struct {
	Binary           string   `json:"binary"`
	Repository       string   `json:"repository"`
	Password         *string  `json:"password,omitempty"`
	PasswordFile     *string  `json:"password_file,omitempty"`
	ResolvedPassword string   `json:"-"`
	Timeout          Duration `json:"timeout"`
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

type EngineConfig struct {
	Type   string `json:"type"`
	Binary string `json:"binary"`
	Socket string `json:"socket"`
}

type ApplicationConfig struct {
	ID                string           `json:"id"`
	Paths             []string         `json:"paths,omitempty"`
	Databases         []DatabaseConfig `json:"databases,omitempty"`
	Hooks             HooksConfig      `json:"hooks,omitempty"`
	Timeout           Duration         `json:"timeout"`
	VerifyAfterBackup bool             `json:"verify_after_backup"`
	Disabled          bool             `json:"disabled"`
}

const (
	HookPreBackup  = "pre_backup"
	HookQuiesce    = "quiesce"
	HookUnquiesce  = "unquiesce"
	HookPostBackup = "post_backup"
)

type HooksConfig struct {
	PreBackup  []CommandConfig `json:"pre_backup,omitempty"`
	Quiesce    []CommandConfig `json:"quiesce,omitempty"`
	Unquiesce  []CommandConfig `json:"unquiesce,omitempty"`
	PostBackup []CommandConfig `json:"post_backup,omitempty"`
}

type HookPhase struct {
	Name     string
	Commands []CommandConfig
}

func (hooks HooksConfig) Phases() []HookPhase {
	return []HookPhase{
		{Name: HookPreBackup, Commands: hooks.PreBackup},
		{Name: HookQuiesce, Commands: hooks.Quiesce},
		{Name: HookUnquiesce, Commands: hooks.Unquiesce},
		{Name: HookPostBackup, Commands: hooks.PostBackup},
	}
}

type CommandConfig struct {
	Binary  string   `json:"binary"`
	Args    []string `json:"args,omitempty"`
	Timeout Duration `json:"timeout"`
}

type DatabaseConfig struct {
	ID               string         `json:"id"`
	Type             string         `json:"type"`
	Binary           string         `json:"binary,omitempty"`
	RestoreBinary    string         `json:"restore_binary,omitempty"`
	Host             string         `json:"host,omitempty"`
	Port             int            `json:"port,omitempty"`
	User             string         `json:"user,omitempty"`
	Name             string         `json:"name,omitempty"`
	Path             string         `json:"path,omitempty"`
	Password         *string        `json:"password,omitempty"`
	PasswordFile     *string        `json:"password_file,omitempty"`
	VerifyCommand    *CommandConfig `json:"verify_command,omitempty"`
	Timeout          Duration       `json:"timeout"`
	ResolvedPassword string         `json:"-"`
}

type Overrides struct{ ConfigPath, Listen, LogLevel, LogFormat *string }
