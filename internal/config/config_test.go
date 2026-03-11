package config

import "testing"

func TestValidateAcceptsAccessTokenAuth(t *testing.T) {
	cfg := Config{
		HomeserverURL: "https://matrix.example.com",
		UserID:        "@bot:example.com",
		AccessToken:   "secret",
		CommandPrefix: "!",
		StatePath:     "data/state.json",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() returned unexpected error: %v", err)
	}
}

func TestValidateAcceptsPasswordAuth(t *testing.T) {
	cfg := Config{
		HomeserverURL: "https://matrix.example.com",
		Username:      "bot",
		Password:      "secret",
		CommandPrefix: "!",
		StatePath:     "data/state.json",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() returned unexpected error: %v", err)
	}
}

func TestValidateRejectsIncompleteAuth(t *testing.T) {
	cfg := Config{
		HomeserverURL: "https://matrix.example.com",
		UserID:        "@bot:example.com",
		CommandPrefix: "!",
		StatePath:     "data/state.json",
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() succeeded without enough auth settings")
	}
}
