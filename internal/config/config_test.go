package config

import (
	"errors"
	"flag"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// envLookup returns a lookupEnv over a map, so tests never read real
// environment variables.
func envLookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// withField returns Default() with one field changed, for comparing against
// a config where exactly one setting was set.
func withField(mutate func(*Config)) Config {
	c := Default()
	mutate(&c)
	return c
}

// TestLoadDefaults checks that no args and no env give Default().
func TestLoadDefaults(t *testing.T) {
	got, err := Load(nil, envLookup(nil))
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if want := Default(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestLoadFlags checks that each flag sets exactly its own field.
func TestLoadFlags(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		set  func(*Config)
	}{
		{"addr", []string{"-addr", ":9090"}, func(c *Config) { c.Addr = ":9090" }},
		{"data", []string{"-data", "/srv/sras"}, func(c *Config) { c.DataDir = "/srv/sras" }},
		{"max-file-size", []string{"-max-file-size", "1024"}, func(c *Config) { c.MaxFileSize = 1024 }},
		{"fsync", []string{"-fsync", "always"}, func(c *Config) { c.Fsync = "always" }},
		{"sync-interval", []string{"-sync-interval", "2s"}, func(c *Config) { c.SyncInterval = 2 * time.Second }},
		{"request-timeout", []string{"-request-timeout", "5s"}, func(c *Config) { c.RequestTimeout = 5 * time.Second }},
		{"log-format", []string{"-log-format", "json"}, func(c *Config) { c.LogFormat = "json" }},
		{"log-level", []string{"-log-level", "debug"}, func(c *Config) { c.LogLevel = slog.LevelDebug }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(tt.args, envLookup(nil))
			if err != nil {
				t.Fatalf("Load(%v): %v", tt.args, err)
			}
			want := withField(tt.set)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

// TestLoadEnv checks that each SRAS_* variable sets its own field;
// SRAS_LOG_LEVEL parses case-insensitively.
func TestLoadEnv(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string
		set  func(*Config)
	}{
		{"addr", map[string]string{"SRAS_ADDR": "127.0.0.1:9091"}, func(c *Config) { c.Addr = "127.0.0.1:9091" }},
		{"data", map[string]string{"SRAS_DATA": "/var/sras"}, func(c *Config) { c.DataDir = "/var/sras" }},
		{"max-file-size", map[string]string{"SRAS_MAX_FILE_SIZE": "2048"}, func(c *Config) { c.MaxFileSize = 2048 }},
		{"fsync", map[string]string{"SRAS_FSYNC": "never"}, func(c *Config) { c.Fsync = "never" }},
		{"sync-interval", map[string]string{"SRAS_SYNC_INTERVAL": "3s"}, func(c *Config) { c.SyncInterval = 3 * time.Second }},
		{"request-timeout", map[string]string{"SRAS_REQUEST_TIMEOUT": "6s"}, func(c *Config) { c.RequestTimeout = 6 * time.Second }},
		{"log-format", map[string]string{"SRAS_LOG_FORMAT": "json"}, func(c *Config) { c.LogFormat = "json" }},
		{"log-level WARN", map[string]string{"SRAS_LOG_LEVEL": "WARN"}, func(c *Config) { c.LogLevel = slog.LevelWarn }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load(nil, envLookup(tt.env))
			if err != nil {
				t.Fatalf("Load(env %v): %v", tt.env, err)
			}
			want := withField(tt.set)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

// TestLoadFlagBeatsEnv checks the precedence flag > env > default for every
// setting at once.
func TestLoadFlagBeatsEnv(t *testing.T) {
	env := map[string]string{
		"SRAS_ADDR":            "127.0.0.1:9091",
		"SRAS_DATA":            "/var/sras",
		"SRAS_MAX_FILE_SIZE":   "2048",
		"SRAS_FSYNC":           "never",
		"SRAS_SYNC_INTERVAL":   "3s",
		"SRAS_REQUEST_TIMEOUT": "6s",
		"SRAS_LOG_FORMAT":      "text",
		"SRAS_LOG_LEVEL":       "warn",
	}
	args := []string{
		"-addr", ":9090",
		"-data", "/srv/sras",
		"-max-file-size", "1024",
		"-fsync", "always",
		"-sync-interval", "2s",
		"-request-timeout", "5s",
		"-log-format", "json",
		"-log-level", "debug",
	}
	want := Config{
		Addr:           ":9090",
		DataDir:        "/srv/sras",
		MaxFileSize:    1024,
		Fsync:          "always",
		SyncInterval:   2 * time.Second,
		RequestTimeout: 5 * time.Second,
		LogFormat:      "json",
		LogLevel:       slog.LevelDebug,
	}
	got, err := Load(args, envLookup(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestLoadErrors checks that invalid input yields an error naming the
// offending flag or variable.
func TestLoadErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		env  map[string]string
		sub  string // the message must name this flag or variable
	}{
		{"bad fsync flag", []string{"-fsync", "sometimes"}, nil, "fsync"},
		{"bad fsync env", nil, map[string]string{"SRAS_FSYNC": "sometimes"}, "fsync"},
		{"bad log-format flag", []string{"-log-format", "xml"}, nil, "log-format"},
		{"bad log-level flag", []string{"-log-level", "loud"}, nil, "log-level"},
		{"zero max-file-size", []string{"-max-file-size", "0"}, nil, "max-file-size"},
		{"negative max-file-size", []string{"-max-file-size", "-1"}, nil, "max-file-size"},
		{"zero sync-interval", []string{"-sync-interval", "0"}, nil, "sync-interval"},
		{"zero request-timeout", []string{"-request-timeout", "0"}, nil, "request-timeout"},
		{"bad sync-interval env", nil, map[string]string{"SRAS_SYNC_INTERVAL": "x"}, "SRAS_SYNC_INTERVAL"},
		{"non-numeric max-file-size", []string{"-max-file-size", "abc"}, nil, "max-file-size"},
		{"positional argument", []string{"extra"}, nil, "extra"},
		{"unknown flag", []string{"-nope"}, nil, "nope"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.args, envLookup(tt.env))
			if err == nil {
				t.Fatalf("Load(%v, env %v): nil error, want one naming %q", tt.args, tt.env, tt.sub)
			}
			if !strings.Contains(err.Error(), tt.sub) {
				t.Fatalf("error %q does not name %q", err, tt.sub)
			}
		})
	}
}

// TestLoadHelp checks that -h surfaces flag.ErrHelp unchanged, so main can
// exit 0, even when an env value is invalid.
func TestLoadHelp(t *testing.T) {
	_, err := Load([]string{"-h"}, envLookup(map[string]string{"SRAS_SYNC_INTERVAL": "x"}))
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Load(-h): %v, want flag.ErrHelp", err)
	}
}

// TestLoadFlagBeatsInvalidEnv checks that a flag overrides an env value that
// would not even parse: the env value is never applied.
func TestLoadFlagBeatsInvalidEnv(t *testing.T) {
	got, err := Load([]string{"-sync-interval", "2s"}, envLookup(map[string]string{"SRAS_SYNC_INTERVAL": "x"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SyncInterval != 2*time.Second {
		t.Fatalf("SyncInterval %v, want 2s", got.SyncInterval)
	}
}

// TestLoadEmptyEnv checks that an empty SRAS_* value counts as unset, so
// SRAS_ADDR= keeps :8080 instead of binding the blank address (port 80).
func TestLoadEmptyEnv(t *testing.T) {
	got, err := Load(nil, envLookup(map[string]string{"SRAS_ADDR": "", "SRAS_DATA": ""}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := Default(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestLoadEnvErrorShowsValue checks that an env parse error names both the
// variable and the rejected value.
func TestLoadEnvErrorShowsValue(t *testing.T) {
	_, err := Load(nil, envLookup(map[string]string{"SRAS_MAX_FILE_SIZE": "64MB"}))
	if err == nil || !strings.Contains(err.Error(), "SRAS_MAX_FILE_SIZE") || !strings.Contains(err.Error(), `"64MB"`) {
		t.Fatalf("error %v, want one naming SRAS_MAX_FILE_SIZE and \"64MB\"", err)
	}
}
