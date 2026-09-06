// Package config loads and validates PIM Backup configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lauritsk/backup/internal/configutil"
	"github.com/lauritsk/backup/internal/secret"
)

const (
	DefaultPath = "/etc/pimbackup/config.json"
	Redacted    = "<redacted>"
)

var accountIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type Duration = configutil.Duration

// Config is the effective PIM Backup configuration.
type Config struct {
	DataDir  string          `json:"data_dir,omitempty"`
	Log      LogConfig       `json:"log,omitempty"`
	Accounts []AccountConfig `json:"accounts"`

	SourcePath string `json:"-"`
}

type LogConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

type AccountConfig struct {
	ID                 string   `json:"id"`
	Protocol           string   `json:"protocol"`
	URL                string   `json:"url,omitempty"`
	Auth               string   `json:"auth,omitempty"`
	Host               string   `json:"host,omitempty"`
	Port               int      `json:"port"`
	TLS                string   `json:"tls"`
	InsecureSkipVerify bool     `json:"insecure_skip_verify"`
	AllowInsecure      bool     `json:"allow_insecure"`
	CAFile             string   `json:"ca_file,omitempty"`
	Username           string   `json:"username"`
	Password           *string  `json:"password,omitempty"`
	PasswordFile       *string  `json:"password_file,omitempty"`
	Token              *string  `json:"token,omitempty"`
	TokenFile          *string  `json:"token_file,omitempty"`
	Mailboxes          []string `json:"mailboxes,omitempty"`
	ExcludeMailboxes   []string `json:"exclude_mailboxes,omitempty"`
	Collections        []string `json:"collections,omitempty"`
	ExcludeCollections []string `json:"exclude_collections,omitempty"`
	Timeout            Duration `json:"timeout"`
	Disabled           bool     `json:"disabled"`

	ResolvedPassword string `json:"-"`
	ResolvedToken    string `json:"-"`
}

// Overrides are CLI values. Pointer fields preserve whether a flag was set.
type Overrides struct {
	ConfigPath *string
	DataDir    *string
	LogLevel   *string
	LogFormat  *string
}

// Load applies defaults, JSON, environment, and CLI overrides in that order.
func Load(overrides Overrides) (Config, error) {
	cfg := defaults()

	configPath, explicitPath := selectConfigPath(overrides.ConfigPath)
	if err := decodeFile(configPath, explicitPath, &cfg); err != nil {
		return Config{}, err
	}
	cfg.SourcePath = configPath
	normalizeAccounts(&cfg)

	if err := applyEnvironment(&cfg); err != nil {
		return Config{}, err
	}
	applyOverrides(&cfg, overrides)
	if err := resolveSecrets(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaults() Config {
	return Config{
		DataDir: "/data",
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

func selectConfigPath(cliPath *string) (string, bool) {
	if cliPath != nil {
		return *cliPath, true
	}
	if value, ok := os.LookupEnv("PIMBACKUP_CONFIG"); ok {
		return value, true
	}
	return DefaultPath, false
}

func decodeFile(filename string, explicit bool, cfg *Config) error {
	return configutil.DecodeFile(filename, !explicit, 4<<20, cfg)
}

func normalizeAccounts(cfg *Config) {
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		if account.Protocol == "" {
			account.Protocol = "imap"
		}
		if account.Protocol == "imap" {
			if account.TLS == "" {
				account.TLS = "implicit"
			}
			if account.Port == 0 {
				switch account.TLS {
				case "implicit":
					account.Port = 993
				default:
					account.Port = 143
				}
			}
		} else if account.Auth == "" {
			account.Auth = "basic"
		}
		if account.Timeout.Duration == 0 {
			account.Timeout.Duration = 30 * time.Second
		}
		if account.Protocol == "imap" && len(account.Mailboxes) == 0 {
			account.Mailboxes = []string{"*"}
		}
		if account.Protocol != "imap" && len(account.Collections) == 0 {
			account.Collections = []string{"*"}
		}
	}
}

func applyEnvironment(cfg *Config) error {
	if value, ok := os.LookupEnv("PIMBACKUP_DATA_DIR"); ok {
		cfg.DataDir = value
	}
	if value, ok := os.LookupEnv("PIMBACKUP_LOG_LEVEL"); ok {
		cfg.Log.Level = value
	}
	if value, ok := os.LookupEnv("PIMBACKUP_LOG_FORMAT"); ok {
		cfg.Log.Format = value
	}
	return nil
}

func applyOverrides(cfg *Config, overrides Overrides) {
	if overrides.DataDir != nil {
		cfg.DataDir = *overrides.DataDir
	}
	if overrides.LogLevel != nil {
		cfg.Log.Level = *overrides.LogLevel
	}
	if overrides.LogFormat != nil {
		cfg.Log.Format = *overrides.LogFormat
	}
}

func resolveSecrets(cfg *Config) error {
	envNames := make(map[string]string)
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		prefix := "PIMBACKUP_ACCOUNT_" + environmentToken(account.ID)
		passwordEnv, tokenEnv := prefix+"_PASSWORD", prefix+"_TOKEN"
		if previous, exists := envNames[passwordEnv]; exists {
			return fmt.Errorf("account IDs %q and %q map to the same password environment variable %s", previous, account.ID, passwordEnv)
		}
		envNames[passwordEnv] = account.ID
		if account.Disabled {
			if err := validateSecretSources(passwordEnv, "accounts."+account.ID+".password", account.Password, account.PasswordFile); err != nil {
				return err
			}
			if err := validateSecretSources(tokenEnv, "accounts."+account.ID+".token", account.Token, account.TokenFile); err != nil {
				return err
			}
			account.Password, account.PasswordFile = nil, nil
			account.Token, account.TokenFile = nil, nil
			continue
		}

		password, passwordSet, err := resolveConfiguredSecret(passwordEnv, "accounts."+account.ID+".password", account.Password, account.PasswordFile)
		if err != nil {
			return err
		}
		if passwordSet {
			account.ResolvedPassword = password
			account.Password = stringPointer(password)
			account.PasswordFile = nil
		}
		token, tokenSet, err := resolveConfiguredSecret(tokenEnv, "accounts."+account.ID+".token", account.Token, account.TokenFile)
		if err != nil {
			return err
		}
		if tokenSet {
			account.ResolvedToken = token
			account.Token = stringPointer(token)
			account.TokenFile = nil
		}
	}
	return nil
}

func resolveConfiguredSecret(envName, configName string, direct, file *string) (string, bool, error) {
	value, set, err := resolveEnvironmentSecret(envName)
	if err != nil || set {
		return value, set, err
	}
	return secret.Resolve(secret.Source{Name: configName, Direct: dereference(direct), DirectSet: direct != nil, File: dereference(file), FileSet: file != nil})
}

func validateSecretSources(envName, configName string, direct, file *string) error {
	if direct != nil && file != nil {
		return fmt.Errorf("secret: %s and %s_file cannot both be set", configName, configName)
	}
	_, directSet := os.LookupEnv(envName)
	_, fileSet := os.LookupEnv(envName + "_FILE")
	if directSet && fileSet {
		return fmt.Errorf("secret: %s and %s_FILE cannot both be set", envName, envName)
	}
	return nil
}

func resolveEnvironmentSecret(name string) (string, bool, error) {
	direct, directSet := os.LookupEnv(name)
	file, fileSet := os.LookupEnv(name + "_FILE")
	return secret.Resolve(secret.Source{
		Name:      name,
		Direct:    direct,
		DirectSet: directSet,
		File:      file,
		FileSet:   fileSet,
	})
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func stringPointer(value string) *string {
	copy := value
	return &copy
}

func environmentToken(id string) string {
	var builder strings.Builder
	for _, r := range strings.ToUpper(id) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

// Validate checks local syntax and semantics without network access.
func (cfg Config) Validate() error {
	var problems []string

	if !filepath.IsAbs(cfg.DataDir) || filepath.Clean(cfg.DataDir) != cfg.DataDir || cfg.DataDir == string(filepath.Separator) || strings.ContainsAny(cfg.DataDir, "\r\n\x00") {
		problems = append(problems, "data_dir must be a clean absolute path other than the filesystem root")
	}
	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, "log.level must be debug, info, warn, or error")
	}
	switch cfg.Log.Format {
	case "json", "text":
	default:
		problems = append(problems, "log.format must be json or text")
	}

	seenIDs := make(map[string]struct{})
	seenEnv := make(map[string]string)
	for i, account := range cfg.Accounts {
		prefix := fmt.Sprintf("accounts[%d]", i)
		if account.ID != "" {
			prefix = "account " + strconv.Quote(account.ID)
		}
		if err := validateAccount(account); err != nil {
			problems = append(problems, prefix+": "+err.Error())
		}
		if _, exists := seenIDs[account.ID]; exists {
			problems = append(problems, prefix+": duplicate account ID")
		}
		seenIDs[account.ID] = struct{}{}
		envName := environmentToken(account.ID)
		if previous, exists := seenEnv[envName]; exists && previous != account.ID {
			problems = append(problems, fmt.Sprintf("account IDs %q and %q have colliding environment names", previous, account.ID))
		}
		seenEnv[envName] = account.ID
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validateAccount(account AccountConfig) error {
	var problems []string
	if !accountIDPattern.MatchString(account.ID) {
		problems = append(problems, "id must match "+accountIDPattern.String())
	}
	if account.Timeout.Duration <= 0 {
		problems = append(problems, "timeout must be greater than zero")
	}
	if account.InsecureSkipVerify && !account.AllowInsecure {
		problems = append(problems, "insecure_skip_verify requires allow_insecure = true")
	}
	if account.CAFile != "" && (!filepath.IsAbs(account.CAFile) || filepath.Clean(account.CAFile) != account.CAFile || strings.ContainsAny(account.CAFile, "\r\n\x00")) {
		problems = append(problems, "ca_file must be a clean absolute path")
	}
	if account.Protocol != "imap" {
		return validateHTTPAccount(account, problems)
	}
	if account.Host == "" {
		problems = append(problems, "host cannot be empty")
	}
	if account.Port < 1 || account.Port > 65535 {
		problems = append(problems, "port must be between 1 and 65535")
	}
	switch account.TLS {
	case "implicit", "starttls":
	case "plain":
		if !account.AllowInsecure {
			problems = append(problems, "tls = plain requires allow_insecure = true")
		}
	default:
		problems = append(problems, "tls must be implicit, starttls, or plain")
	}
	if account.Username == "" {
		problems = append(problems, "username cannot be empty")
	}
	if account.ResolvedPassword == "" && !account.Disabled {
		problems = append(problems, "password or password_file is required")
	}
	for _, pattern := range append(append([]string(nil), account.Mailboxes...), account.ExcludeMailboxes...) {
		if pattern == "" {
			problems = append(problems, "mailbox patterns cannot be empty")
			continue
		}
		if _, err := path.Match(pattern, "mailbox"); err != nil {
			problems = append(problems, fmt.Sprintf("invalid mailbox pattern %q: %v", pattern, err))
		}
	}
	return errors.Join(stringsToErrors(problems)...)
}

func validateHTTPAccount(account AccountConfig, problems []string) error {
	switch account.Protocol {
	case "jmap", "carddav", "caldav":
	default:
		problems = append(problems, "protocol must be imap, jmap, carddav, or caldav")
	}
	if account.URL != "" {
		parsed, err := url.Parse(account.URL)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			problems = append(problems, "url must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
		} else if parsed.Scheme != "https" && !account.AllowInsecure {
			problems = append(problems, "an HTTP url requires allow_insecure = true")
		}
	} else {
		domain := account.Host
		if domain == "" {
			if at := strings.LastIndexByte(account.Username, '@'); at >= 0 {
				domain = account.Username[at+1:]
			}
		}
		if domain == "" {
			problems = append(problems, "autodiscovery requires host or an email-style username when url is omitted")
		} else if !validDiscoveryDomain(domain) {
			problems = append(problems, "autodiscovery host must be a DNS name without a scheme, credentials, port, path, query, or fragment")
		}
	}
	switch account.Auth {
	case "basic":
		if account.Username == "" {
			problems = append(problems, "username is required for basic authentication")
		}
		if account.ResolvedPassword == "" && !account.Disabled {
			problems = append(problems, "password or password_file is required for basic authentication")
		}
	case "bearer":
		if account.ResolvedToken == "" && !account.Disabled {
			problems = append(problems, "token or token_file is required for bearer authentication")
		}
	default:
		problems = append(problems, "auth must be basic or bearer")
	}
	for _, pattern := range append(append([]string(nil), account.Collections...), account.ExcludeCollections...) {
		if pattern == "" {
			problems = append(problems, "collection patterns cannot be empty")
			continue
		}
		if _, err := path.Match(pattern, "collection"); err != nil {
			problems = append(problems, fmt.Sprintf("invalid collection pattern %q: %v", pattern, err))
		}
	}
	return errors.Join(stringsToErrors(problems)...)
}

func validDiscoveryDomain(domain string) bool {
	if strings.TrimSpace(domain) != domain || strings.ContainsAny(domain, "/:@?#\\") {
		return false
	}
	parsed, err := url.Parse("https://" + domain)
	return err == nil && parsed.Host == domain && parsed.Hostname() != "" && parsed.Port() == "" && parsed.Path == ""
}

func stringsToErrors(values []string) []error {
	out := make([]error, 0, len(values))
	for _, value := range values {
		out = append(out, errors.New(value))
	}
	return out
}

// EnabledAccounts returns configured accounts that are not disabled.
func (cfg Config) EnabledAccounts() []AccountConfig {
	accounts := make([]AccountConfig, 0, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		if !account.Disabled {
			accounts = append(accounts, account)
		}
	}
	return accounts
}

// Account returns a configured account by ID.
func (cfg Config) Account(id string) (AccountConfig, bool) {
	for _, account := range cfg.Accounts {
		if account.ID == id {
			return account, true
		}
	}
	return AccountConfig{}, false
}

// RedactedCopy returns effective configuration that is safe to print.
func (cfg Config) RedactedCopy() Config {
	copy := cfg
	copy.Accounts = append([]AccountConfig(nil), cfg.Accounts...)
	for i := range copy.Accounts {
		if cfg.Accounts[i].ResolvedPassword != "" {
			copy.Accounts[i].Password = stringPointer(Redacted)
		} else {
			copy.Accounts[i].Password = nil
		}
		copy.Accounts[i].PasswordFile = nil
		copy.Accounts[i].ResolvedPassword = ""
		if cfg.Accounts[i].ResolvedToken != "" {
			copy.Accounts[i].Token = stringPointer(Redacted)
		} else {
			copy.Accounts[i].Token = nil
		}
		copy.Accounts[i].TokenFile = nil
		copy.Accounts[i].ResolvedToken = ""
	}
	return copy
}
