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
	envUsername           = "MATRIX_USERNAME"
	envPassword           = "MATRIX_PASSWORD"
	envStatePath          = "MATRIX_STATE_PATH"
	envA2AAgentURL        = "A2A_AGENT_URL"
	envSessionIdleTimeout = "BOT_SESSION_IDLE_TIMEOUT"
)

type Config struct {
	HomeserverURL      string
	Username           string
	Password           string
	StatePath          string
	A2AAgentURL        string
	SessionIdleTimeout time.Duration
}

func FromEnv() (Config, error) {
	cfg := Config{
		HomeserverURL: strings.TrimSpace(os.Getenv(envHomeserverURL)),
		Username:      strings.TrimSpace(os.Getenv(envUsername)),
		Password:      strings.TrimSpace(os.Getenv(envPassword)),
		StatePath:     strings.TrimSpace(os.Getenv(envStatePath)),
		A2AAgentURL:   strings.TrimSpace(os.Getenv(envA2AAgentURL)),
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

	hasPasswordAuth := c.Username != "" && c.Password != ""

	switch {
	case hasPasswordAuth:
	case c.Username != "" || c.Password != "":
		problems = append(problems, "set both "+envUsername+" and "+envPassword+" for password auth")
	default:
		problems = append(problems, "set both "+envUsername+" and "+envPassword)
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
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
