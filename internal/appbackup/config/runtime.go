package config

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
	return &command
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
	result.Restic.PasswordFile = nil
	result.Restic.ResolvedPassword = ""
	result.Restic.ResolvedPasswordFile = ""
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
			database.PasswordFile = nil
			database.ResolvedPassword = ""
			database.ResolvedPasswordFile = ""
		}
	}
	return result
}
