package config

import "github.com/lauritsk/backup/internal/configutil"

const (
	DefaultPath = "/etc/appbackup/config.json"
	Redacted    = "<redacted>"
)

type Duration = configutil.Duration

type Config struct {
	DataDir      string              `json:"data_dir,omitempty"`
	Restic       ResticConfig        `json:"restic"`
	Log          LogConfig           `json:"log,omitempty"`
	Applications []ApplicationConfig `json:"applications"`
	SourcePath   string              `json:"-"`
}

type ResticConfig struct {
	Binary               string   `json:"binary,omitempty"`
	Password             *string  `json:"password,omitempty"`
	PasswordFile         *string  `json:"password_file,omitempty"`
	ResolvedPassword     string   `json:"-"`
	ResolvedPasswordFile string   `json:"-"`
	Timeout              Duration `json:"timeout,omitempty"`
}

type LogConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
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
	ID                   string         `json:"id"`
	Type                 string         `json:"type"`
	Binary               string         `json:"binary,omitempty"`
	RestoreBinary        string         `json:"restore_binary,omitempty"`
	Host                 string         `json:"host,omitempty"`
	Port                 int            `json:"port,omitempty"`
	User                 string         `json:"user,omitempty"`
	Name                 string         `json:"name,omitempty"`
	Path                 string         `json:"path,omitempty"`
	Password             *string        `json:"password,omitempty"`
	PasswordFile         *string        `json:"password_file,omitempty"`
	VerifyCommand        *CommandConfig `json:"verify_command,omitempty"`
	Timeout              Duration       `json:"timeout"`
	ResolvedPassword     string         `json:"-"`
	ResolvedPasswordFile string         `json:"-"`
}

type Overrides struct{ ConfigPath, DataDir, LogLevel, LogFormat *string }
