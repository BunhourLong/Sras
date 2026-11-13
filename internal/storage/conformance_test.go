package storage_test

import (
	"testing"

	"sras/internal/storage"
	"sras/internal/storage/enginetest"
)

// TestBitcaskConformance runs the shared engine conformance suite against
// Bitcask. The small MaxFileSize makes Put rotate files, so Merge and reopen
// see real immutable files; FsyncNever keeps the suite fast.
func TestBitcaskConformance(t *testing.T) {
	open := func(t *testing.T, dir string) storage.Engine {
		t.Helper()
		b, err := storage.Open(storage.Options{
			Dir:         dir,
			MaxFileSize: 4 << 10,
			Fsync:       storage.FsyncNever,
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	enginetest.RunEngineTests(t, open)
}
