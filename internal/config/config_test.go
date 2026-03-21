package config

import (
	"strings"
	"testing"
)

func TestValidateAcceptsPasswordAuth(t *testing.T) {
	cfg := Config{
		HomeserverURL:  "https://matrix.example.com",
		Username:       "bot",
		Password:       "secret",
		StatePath:      "data/state.json",
		UpstreamA2AURL: "http://127.0.0.1:9999",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() returned unexpected error: %v", err)
	}
}

func TestValidateRejectsIncompleteAuth(t *testing.T) {
	cfg := Config{
		HomeserverURL:  "https://matrix.example.com",
		Username:       "bot",
		StatePath:      "data/state.json",
		UpstreamA2AURL: "http://127.0.0.1:9999",
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() succeeded without enough auth settings")
	}
	if !strings.Contains(err.Error(), envUsername) || !strings.Contains(err.Error(), envPassword) {
		t.Fatalf("Validate() error = %v, want mention of %s and %s", err, envUsername, envPassword)
	}
}

func TestValidateRequiresA2AURL(t *testing.T) {
	cfg := Config{
		HomeserverURL: "https://matrix.example.com",
		Username:      "bot",
		Password:      "secret",
		StatePath:     "data/state.json",
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() succeeded without an A2A URL")
	}

	if !strings.Contains(err.Error(), envUpstreamA2AURL) {
		t.Fatalf("Validate() error = %v, want mention of %s", err, envUpstreamA2AURL)
	}
}
