// Package config defines the server's runtime configuration. Every setting
// has a default, a command-line flag and an SRAS_* environment variable;
// Load assembles them with precedence flag > env > default and validates
// the result.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Config is the whole runtime configuration, assembled in main.
type Config struct {
	// Addr is the HTTP listen address.
	Addr string
	// DataDir holds the numbered data files.
	DataDir string
	// MaxFileSize is the size at which the active data file is rotated.
	MaxFileSize int64
	// Fsync is the durability policy: "always", "interval" or "never".
	Fsync string
	// SyncInterval is the flush period when Fsync is "interval".
	SyncInterval time.Duration
	// RequestTimeout bounds every HTTP request's context.
	RequestTimeout time.Duration
	// LogFormat is the structured-log format: "text" or "json".
	LogFormat string
	// LogLevel is the minimum level that gets logged.
	LogLevel slog.Level
}

// Default returns the configuration used when no flags or environment
// variables are given.
func Default() Config {
	return Config{
		Addr:           ":8080",
		DataDir:        "./data",
		MaxFileSize:    64 << 20,
		Fsync:          "interval",
		SyncInterval:   time.Second,
		RequestTimeout: 30 * time.Second,
		LogFormat:      "text",
		LogLevel:       slog.LevelInfo,
	}
}

// Load builds the configuration from args (as parsed by an "sras" FlagSet,
// never the global one) layered over SRAS_* environment variables looked up
// via lookupEnv, with precedence flag > env > default. It validates the
// result: fsync must be "always", "interval" or "never", log-format "text"
// or "json", and max-file-size, sync-interval and request-timeout must be
// positive; an invalid value is an error naming the flag. -h/-help returns
// flag.ErrHelp unchanged.
func Load(args []string, lookupEnv func(string) (string, bool)) (Config, error) {
	cfg := Default()
	fs := flag.NewFlagSet("sras", flag.ContinueOnError)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	fs.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory")
	fs.Int64Var(&cfg.MaxFileSize, "max-file-size", cfg.MaxFileSize, "max data file size in bytes")
	fs.StringVar(&cfg.Fsync, "fsync", cfg.Fsync, `fsync policy: "always", "interval" or "never"`)
	fs.DurationVar(&cfg.SyncInterval, "sync-interval", cfg.SyncInterval, "flush period when fsync is \"interval\"")
	fs.DurationVar(&cfg.RequestTimeout, "request-timeout", cfg.RequestTimeout, "per-request timeout")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, `log format: "text" or "json"`)
	fs.TextVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level (debug, info, warn, error)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	// env fills only flags not given on the command line, so a flag wins
	// even over an env value that would not parse; empty counts as unset
	onCmdLine := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { onCmdLine[f.Name] = true })
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		name := envName(f.Name)
		v, ok := lookupEnv(name)
		if envErr != nil || onCmdLine[f.Name] || !ok || v == "" {
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			envErr = fmt.Errorf("%s: invalid value %q: %w", name, v, err)
		}
	})
	if envErr != nil {
		return Config{}, envErr
	}

	switch {
	case cfg.Fsync != "always" && cfg.Fsync != "interval" && cfg.Fsync != "never":
		return Config{}, fmt.Errorf("invalid -fsync %q: must be always, interval or never", cfg.Fsync)
	case cfg.LogFormat != "text" && cfg.LogFormat != "json":
		return Config{}, fmt.Errorf("invalid -log-format %q: must be text or json", cfg.LogFormat)
	case cfg.MaxFileSize <= 0:
		return Config{}, fmt.Errorf("invalid -max-file-size %d: must be > 0", cfg.MaxFileSize)
	case cfg.SyncInterval <= 0:
		return Config{}, fmt.Errorf("invalid -sync-interval %v: must be > 0", cfg.SyncInterval)
	case cfg.RequestTimeout <= 0:
		return Config{}, fmt.Errorf("invalid -request-timeout %v: must be > 0", cfg.RequestTimeout)
	}
	return cfg, nil
}

// envName maps a flag name to its SRAS_* environment variable
// (max-file-size -> SRAS_MAX_FILE_SIZE).
func envName(flag string) string {
	return "SRAS_" + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}
