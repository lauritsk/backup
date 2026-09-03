package config

import "path/filepath"

func (cfg Config) EnabledApplications() []ApplicationConfig {
	var result []ApplicationConfig
	for _, app := range cfg.Applications {
		if !app.Disabled {
			result = append(result, app)
		}
	}
	return result
}
func (cfg Config) EffectiveVerificationCommand(database DatabaseConfig) *CommandConfig {
	if database.VerifyCommand == nil {
		return nil
	}
	command := *database.VerifyCommand
	command.Args = append([]string(nil), command.Args...)
	engineType := containerEngineType(command.Binary)
	if engineType == "" || cfg.Engine == nil || cfg.Engine.Type != engineType {
		return &command
	}
	option := "--host"
	if engineType == "podman" {
		option = "--url"
	}
	command.Args = append([]string{option, "unix://" + cfg.Engine.Socket}, command.Args...)
	return &command
}
func containerEngineType(binary string) string {
	name := filepath.Base(binary)
	if name == "docker" || name == "podman" {
		return name
	}
	return ""
}

func (cfg Config) Application(id string) (ApplicationConfig, bool) {
	for _, app := range cfg.Applications {
		if app.ID == id {
			return app, true
		}
	}
	return ApplicationConfig{}, false
}
func (cfg Config) RedactedCopy() Config {
	result := cfg
	result.Restic = cfg.Restic
	if cfg.Restic.ResolvedPassword != "" {
		result.Restic.Password = pointer(Redacted)
	} else {
		result.Restic.Password = nil
	}
	result.Restic.PasswordFile, result.Restic.ResolvedPassword = nil, ""
	result.Server = cfg.Server
	if cfg.Server.ResolvedAuthToken != "" {
		result.Server.AuthToken = pointer(Redacted)
	} else {
		result.Server.AuthToken = nil
	}
	result.Server.AuthTokenFile, result.Server.ResolvedAuthToken = nil, ""
	result.Applications = append([]ApplicationConfig(nil), cfg.Applications...)
	for i := range result.Applications {
		result.Applications[i].Databases = append([]DatabaseConfig(nil), cfg.Applications[i].Databases...)
		for j := range result.Applications[i].Databases {
			database := &result.Applications[i].Databases[j]
			if database.ResolvedPassword != "" {
				database.Password = pointer(Redacted)
			} else {
				database.Password = nil
			}
			database.PasswordFile, database.ResolvedPassword = nil, ""
		}
	}
	return result
}
