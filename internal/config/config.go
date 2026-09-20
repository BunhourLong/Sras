// Package config holds the server and storage configuration (data dir, listen
// address, max file size, fsync policy).
package config

import "time"

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
}

// Default returns the configuration used when no flags are given.
func Default() Config {
	return Config{
		Addr:         ":8080",
		DataDir:      "./data",
		MaxFileSize:  64 << 20,
		Fsync:        "interval",
		SyncInterval: time.Second,
	}
}
