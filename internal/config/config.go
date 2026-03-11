package config

import (
	"errors"
	"os"
	"strings"
)

const (
	defaultCommandPrefix = "!"
	defaultStatePath     = "data/state.json"

	envHomeserverURL = "MATRIX_HOMESERVER_URL"
	envUserID        = "MATRIX_USER_ID"
	envAccessToken   = "MATRIX_ACCESS_TOKEN"
	envUsername      = "MATRIX_USERNAME"
	envPassword      = "MATRIX_PASSWORD"
	envCommandPrefix = "MATRIX_COMMAND_PREFIX"
	envAutoJoin      = "MATRIX_AUTO_JOIN_INVITES"
	envStatePath     = "MATRIX_STATE_PATH"
)

type Config struct {
	HomeserverURL   string
	UserID          string
	AccessToken     string
	Username        string
	Password        string
	CommandPrefix   string
	AutoJoinInvites bool
	StatePath       string
}

func FromEnv() (Config, error) {
	cfg := Config{
		HomeserverURL:   strings.TrimSpace(os.Getenv(envHomeserverURL)),
		UserID:          strings.TrimSpace(os.Getenv(envUserID)),
		AccessToken:     strings.TrimSpace(os.Getenv(envAccessToken)),
		Username:        strings.TrimSpace(os.Getenv(envUsername)),
		Password:        strings.TrimSpace(os.Getenv(envPassword)),
		CommandPrefix:   strings.TrimSpace(os.Getenv(envCommandPrefix)),
		AutoJoinInvites: readBoolEnv(envAutoJoin, true),
		StatePath:       strings.TrimSpace(os.Getenv(envStatePath)),
	}
	if cfg.CommandPrefix == "" {
		cfg.CommandPrefix = defaultCommandPrefix
	}
	if cfg.StatePath == "" {
		cfg.StatePath = defaultStatePath
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	var problems []string

	if c.HomeserverURL == "" {
		problems = append(problems, envHomeserverURL+" is required")
	}
	if c.CommandPrefix == "" {
		problems = append(problems, "command prefix must not be empty")
	}
	if c.StatePath == "" {
		problems = append(problems, "state path must not be empty")
	}

	hasAccessTokenAuth := c.UserID != "" && c.AccessToken != ""
	hasPasswordAuth := c.Username != "" && c.Password != ""

	switch {
	case hasAccessTokenAuth:
	case hasPasswordAuth:
	case c.AccessToken != "" || c.UserID != "":
		problems = append(problems, "set both "+envUserID+" and "+envAccessToken+" for access-token auth")
	case c.Username != "" || c.Password != "":
		problems = append(problems, "set both "+envUsername+" and "+envPassword+" for password auth")
	default:
		problems = append(problems, "set "+envUserID+" + "+envAccessToken+" or "+envUsername+" + "+envPassword)
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

func (c Config) UsingAccessToken() bool {
	return c.UserID != "" && c.AccessToken != ""
}

func readBoolEnv(name string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
