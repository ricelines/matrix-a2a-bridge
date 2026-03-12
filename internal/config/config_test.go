package config

import (
	"strings"
	"testing"
	"time"
)

func TestValidateAcceptsAccessTokenAuth(t *testing.T) {
	cfg := Config{
		HomeserverURL:      "https://matrix.example.com",
		UserID:             "@bot:example.com",
		AccessToken:        "secret",
		StatePath:          "data/state.json",
		A2AAgentURL:        "http://127.0.0.1:9999",
		SessionIdleTimeout: 5 * time.Minute,
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() returned unexpected error: %v", err)
	}
}

func TestValidateAcceptsPasswordAuth(t *testing.T) {
	cfg := Config{
		HomeserverURL:      "https://matrix.example.com",
		Username:           "bot",
		Password:           "secret",
		StatePath:          "data/state.json",
		A2AAgentURL:        "http://127.0.0.1:9999",
		SessionIdleTimeout: 5 * time.Minute,
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() returned unexpected error: %v", err)
	}
}

func TestValidateRejectsIncompleteAuth(t *testing.T) {
	cfg := Config{
		HomeserverURL:      "https://matrix.example.com",
		UserID:             "@bot:example.com",
		StatePath:          "data/state.json",
		A2AAgentURL:        "http://127.0.0.1:9999",
		SessionIdleTimeout: 5 * time.Minute,
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() succeeded without enough auth settings")
	}
}

func TestValidateRequiresA2AURLAndPositiveIdleTimeout(t *testing.T) {
	cfg := Config{
		HomeserverURL:      "https://matrix.example.com",
		Username:           "bot",
		Password:           "secret",
		StatePath:          "data/state.json",
		SessionIdleTimeout: 0,
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() succeeded without an A2A URL and idle timeout")
	}

	if !strings.Contains(err.Error(), envA2AAgentURL) {
		t.Fatalf("Validate() error = %v, want mention of %s", err, envA2AAgentURL)
	}
	if !strings.Contains(err.Error(), envSessionIdleTimeout) {
		t.Fatalf("Validate() error = %v, want mention of %s", err, envSessionIdleTimeout)
	}
}
