package config

import (
	"errors"
	"os"
	"strings"
)

const (
	defaultStatePath = "data/state.json"

	envHomeserverURL  = "MATRIX_HOMESERVER_URL"
	envUsername       = "MATRIX_USERNAME"
	envPassword       = "MATRIX_PASSWORD"
	envStatePath      = "MATRIX_STATE_PATH"
	envUpstreamA2AURL = "UPSTREAM_A2A_URL"
)

type Config struct {
	HomeserverURL  string
	Username       string
	Password       string
	StatePath      string
	UpstreamA2AURL string
}

func FromEnv() (Config, error) {
	cfg := Config{
		HomeserverURL:  strings.TrimSpace(os.Getenv(envHomeserverURL)),
		Username:       strings.TrimSpace(os.Getenv(envUsername)),
		Password:       strings.TrimSpace(os.Getenv(envPassword)),
		StatePath:      strings.TrimSpace(os.Getenv(envStatePath)),
		UpstreamA2AURL: strings.TrimSpace(os.Getenv(envUpstreamA2AURL)),
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
	if c.StatePath == "" {
		problems = append(problems, "state path must not be empty")
	}
	if c.UpstreamA2AURL == "" {
		problems = append(problems, envUpstreamA2AURL+" is required")
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
