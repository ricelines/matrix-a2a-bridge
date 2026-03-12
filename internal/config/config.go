package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	defaultStatePath          = "data/state.json"
	defaultSessionIdleTimeout = 10 * time.Minute

	envHomeserverURL      = "MATRIX_HOMESERVER_URL"
	envUserID             = "MATRIX_USER_ID"
	envAccessToken        = "MATRIX_ACCESS_TOKEN"
	envUsername           = "MATRIX_USERNAME"
	envPassword           = "MATRIX_PASSWORD"
	envAutoJoin           = "MATRIX_AUTO_JOIN_INVITES"
	envStatePath          = "MATRIX_STATE_PATH"
	envA2AAgentURL        = "A2A_AGENT_URL"
	envSessionIdleTimeout = "BOT_SESSION_IDLE_TIMEOUT"
)

type Config struct {
	HomeserverURL      string
	UserID             string
	AccessToken        string
	Username           string
	Password           string
	AutoJoinInvites    bool
	StatePath          string
	A2AAgentURL        string
	SessionIdleTimeout time.Duration
}

func FromEnv() (Config, error) {
	cfg := Config{
		HomeserverURL:   strings.TrimSpace(os.Getenv(envHomeserverURL)),
		UserID:          strings.TrimSpace(os.Getenv(envUserID)),
		AccessToken:     strings.TrimSpace(os.Getenv(envAccessToken)),
		Username:        strings.TrimSpace(os.Getenv(envUsername)),
		Password:        strings.TrimSpace(os.Getenv(envPassword)),
		AutoJoinInvites: readBoolEnv(envAutoJoin, true),
		StatePath:       strings.TrimSpace(os.Getenv(envStatePath)),
		A2AAgentURL:     strings.TrimSpace(os.Getenv(envA2AAgentURL)),
	}
	if cfg.StatePath == "" {
		cfg.StatePath = defaultStatePath
	}

	idleTimeout, err := readDurationEnv(envSessionIdleTimeout, defaultSessionIdleTimeout)
	if err != nil {
		return Config{}, err
	}
	cfg.SessionIdleTimeout = idleTimeout

	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	var problems []string

	if c.HomeserverURL == "" {
		problems = append(problems, envHomeserverURL+" is required")
	}
	if c.StatePath == "" {
		problems = append(problems, "state path must not be empty")
	}
	if c.A2AAgentURL == "" {
		problems = append(problems, envA2AAgentURL+" is required")
	}
	if c.SessionIdleTimeout <= 0 {
		problems = append(problems, envSessionIdleTimeout+" must be greater than zero")
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

func readDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return value, nil
}
