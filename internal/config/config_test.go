package config

import (
	"strings"
	"testing"
	"time"
)

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
		Username:           "bot",
		StatePath:          "data/state.json",
		A2AAgentURL:        "http://127.0.0.1:9999",
		SessionIdleTimeout: 5 * time.Minute,
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() succeeded without enough auth settings")
	}
	if !strings.Contains(err.Error(), envUsername) || !strings.Contains(err.Error(), envPassword) {
		t.Fatalf("Validate() error = %v, want mention of %s and %s", err, envUsername, envPassword)
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
