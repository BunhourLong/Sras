// Package storage implements the Bitcask engine behind the Engine interface.
// It knows only []byte keys and values: no JSON, HTTP or service types.
package storage

import (
	"errors"
	"log/slog"
	"time"
)

// Engine is the key/value store the rest of the database is built on.
type Engine interface {
	Get(key []byte) ([]byte, error) // ErrNotFound if missing
	Put(key, value []byte) error
	Delete(key []byte) error
	Scan(prefix []byte, fn func(key, value []byte) bool) error
	Merge() error // compaction
	Close() error
}

// Sentinel errors returned by the storage layer.
var (
	ErrNotFound       = errors.New("storage: key not found")
	ErrCorrupt        = errors.New("storage: corrupt record")
	ErrClosed         = errors.New("storage: engine is closed")
	ErrLocked         = errors.New("storage: data dir is locked by another engine")
	ErrNotImplemented = errors.New("storage: not implemented")
)

// FsyncPolicy decides when a write is flushed to disk.
type FsyncPolicy string

const (
	FsyncAlways   FsyncPolicy = "always"
	FsyncInterval FsyncPolicy = "interval"
	FsyncNever    FsyncPolicy = "never"
)

// Defaults applied by Open when an option is left zero.
const (
	DefaultMaxFileSize  int64 = 64 << 20 // 64 MB
	DefaultSyncInterval       = time.Second
)

// Options configure Open.
type Options struct {
	// Dir is the data directory holding the numbered data files.
	Dir string
	// MaxFileSize is the size at which the active file is rotated.
	MaxFileSize int64
	// Fsync is the durability policy for writes.
	Fsync FsyncPolicy
	// SyncInterval is the flush period when Fsync is FsyncInterval.
	SyncInterval time.Duration
	// Logger receives recovery and compaction warnings.
	Logger *slog.Logger
}
