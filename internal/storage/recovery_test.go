package storage

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// putAll writes key/value pairs in order and fails the test on any error.
func putAll(t *testing.T, b *Bitcask, kv ...string) {
	t.Helper()
	for i := 0; i < len(kv); i += 2 {
		if err := b.Put([]byte(kv[i]), []byte(kv[i+1])); err != nil {
			t.Fatal(err)
		}
	}
}

// wantGet checks that key reads back as want, or is missing when want is nil.
func wantGet(t *testing.T, b *Bitcask, key string, want []byte) {
	t.Helper()
	got, err := b.Get([]byte(key))
	if want == nil {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("get %s = %q, %v; want ErrNotFound", key, got, err)
		}
		return
	}
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("get %s = %q, %v; want %q", key, got, err, want)
	}
}

// writeRecords writes a data file with the given raw records plus tail bytes.
func writeRecords(t *testing.T, dir string, id uint32, tail []byte, recs ...Record) {
	t.Helper()
	var buf []byte
	for _, r := range recs {
		enc, err := encodeRecord(r.Timestamp, r.Key, r.Value, r.Tombstone)
		if err != nil {
			t.Fatal(err)
		}
		buf = append(buf, enc...)
	}
	buf = append(buf, tail...)
	if err := os.WriteFile(datafilePath(dir, id), buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestRecoveryReopen(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir, FsyncNever)
	const n = 100
	for i := 0; i < n; i++ {
		putAll(t, b, fmt.Sprintf("k/%d", i), fmt.Sprintf("v%d", i))
	}
	// Overwrites: the last one must win after replay.
	putAll(t, b, "k/0", "a", "k/0", "b", "k/0", "latest")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b = openTest(t, dir, FsyncNever)
	if got := b.keydir.len(); got != n {
		t.Fatalf("keydir has %d keys, want %d", got, n)
	}
	wantGet(t, b, "k/0", []byte("latest"))
	for i := 1; i < n; i++ {
		wantGet(t, b, fmt.Sprintf("k/%d", i), fmt.Appendf(nil, "v%d", i))
	}
}

// Tombstones written by a later record remove the key on replay.
func TestRecoveryTombstone(t *testing.T) {
	dir := t.TempDir()
	writeRecords(t, dir, 1, nil,
		Record{Timestamp: 1, Key: []byte("a"), Value: []byte("1")},
		Record{Timestamp: 2, Key: []byte("b"), Value: []byte("2")},
		Record{Timestamp: 3, Key: []byte("a"), Tombstone: true},
	)
	b := openTest(t, dir, FsyncNever)
	wantGet(t, b, "a", nil)
	wantGet(t, b, "b", []byte("2"))
}

func TestRecoveryCorruptActiveTail(t *testing.T) {
	recs := []Record{
		{Timestamp: 1, Key: []byte("a"), Value: []byte("one")},
		{Timestamp: 2, Key: []byte("b"), Value: []byte("two")},
		{Timestamp: 3, Key: []byte("c"), Value: []byte("three")},
	}
	recSize := func(r Record) int64 { return int64(headerSize + len(r.Key) + len(r.Value)) }
	endOf2 := recSize(recs[0]) + recSize(recs[1])

	tests := []struct {
		name string
		// damage corrupts the file at path and returns the expected size after
		// recovery, plus the keys that must survive.
		damage func(t *testing.T, path string) (int64, []string)
	}{
		{"truncated last record", func(t *testing.T, path string) (int64, []string) {
			if err := os.Truncate(path, fileSize(t, path)-5); err != nil {
				t.Fatal(err)
			}
			return endOf2, []string{"a", "b"}
		}},
		{"torn header", func(t *testing.T, path string) (int64, []string) {
			if err := os.Truncate(path, endOf2+7); err != nil {
				t.Fatal(err)
			}
			return endOf2, []string{"a", "b"}
		}},
		{"garbage tail", func(t *testing.T, path string) (int64, []string) {
			size := fileSize(t, path)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			// Claims a huge key: must be rejected without allocating it.
			f.Write([]byte{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 0xff, 0xff, 0xff, 0x7f, 0, 0, 0, 0, 9, 9})
			f.Close()
			return size, []string{"a", "b", "c"}
		}},
		{"flipped byte in the middle", func(t *testing.T, path string) (int64, []string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data[recSize(recs[0])+headerSize] ^= 0xff // key byte of record 2
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			return recSize(recs[0]), []string{"a"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRecords(t, dir, 1, nil, recs...)
			path := datafilePath(dir, 1)
			wantSize, alive := tt.damage(t, path)

			var logs bytes.Buffer
			b, err := Open(Options{Dir: dir, Fsync: FsyncNever, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { b.Close() })

			if got := fileSize(t, path); got != wantSize {
				t.Fatalf("file size %d, want %d", got, wantSize)
			}
			if got := b.keydir.len(); got != len(alive) {
				t.Fatalf("keydir has %d keys, want %d", got, len(alive))
			}
			for _, r := range recs {
				want := []byte(nil)
				for _, k := range alive {
					if k == string(r.Key) {
						want = r.Value
					}
				}
				wantGet(t, b, string(r.Key), want)
			}
			if !strings.Contains(logs.String(), "truncated corrupt tail") || !strings.Contains(logs.String(), "bytesDropped") {
				t.Fatalf("no truncation warning logged: %q", logs.String())
			}

			// The next Put appends right after the last valid record.
			putAll(t, b, "d", "four")
			wantGet(t, b, "d", []byte("four"))
			if got := b.active.size; got != wantSize+headerSize+1+4 {
				t.Fatalf("active size after put %d, want %d", got, wantSize+headerSize+5)
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			b = openTest(t, dir, FsyncNever)
			wantGet(t, b, "d", []byte("four"))
		})
	}
}

func TestRecoveryCorruptImmutableFile(t *testing.T) {
	dir := t.TempDir()
	writeRecords(t, dir, 1, []byte("garbage"), Record{Timestamp: 1, Key: []byte("a"), Value: []byte("1")})
	writeRecords(t, dir, 2, nil, Record{Timestamp: 2, Key: []byte("b"), Value: []byte("2")})
	size := fileSize(t, datafilePath(dir, 1))

	if _, err := Open(Options{Dir: dir}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("open: err %v, want ErrCorrupt", err)
	}
	if got := fileSize(t, datafilePath(dir, 1)); got != size {
		t.Fatalf("immutable file was modified: size %d, want %d", got, size)
	}
	// A failed Open must release the lock.
	if b, err := Open(Options{Dir: dir}); !errors.Is(err, ErrCorrupt) {
		if err == nil {
			b.Close()
		}
		t.Fatalf("second open: err %v, want ErrCorrupt", err)
	}
}

func TestRecoveryEmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeRecords(t, dir, 1, nil)
	b := openTest(t, dir, FsyncNever)
	if got := b.keydir.len(); got != 0 {
		t.Fatalf("keydir has %d keys, want 0", got)
	}
	putAll(t, b, "a", "1")
	wantGet(t, b, "a", []byte("1"))
}

// Later records win on replay even across files, and a record from the
// "future" (clock jumped back since) must not beat a new write.
func TestRecoveryMonotonicTimestamps(t *testing.T) {
	dir := t.TempDir()
	future := int64(1) << 62
	writeRecords(t, dir, 1, nil, Record{Timestamp: future, Key: []byte("a"), Value: []byte("old")})

	b := openTest(t, dir, FsyncNever)
	putAll(t, b, "a", "new")
	if e, _ := b.keydir.get("a"); e.timestamp <= future {
		t.Fatalf("put timestamp %d, want > %d", e.timestamp, future)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b = openTest(t, dir, FsyncNever)
	wantGet(t, b, "a", []byte("new"))
}

// An older record replayed later (as merged files will be) loses to the newer
// one already in the keydir.
func TestRecoveryOlderRecordLoses(t *testing.T) {
	dir := t.TempDir()
	writeRecords(t, dir, 1, nil, Record{Timestamp: 10, Key: []byte("a"), Value: []byte("new")})
	writeRecords(t, dir, 2, nil,
		Record{Timestamp: 5, Key: []byte("a"), Value: []byte("old")},
		Record{Timestamp: 6, Key: []byte("a"), Tombstone: true},
	)
	b := openTest(t, dir, FsyncNever)
	wantGet(t, b, "a", []byte("new"))
}

func TestOpenLocksDir(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir, FsyncNever)
	if b2, err := Open(Options{Dir: dir}); !errors.Is(err, ErrLocked) {
		if err == nil {
			b2.Close()
		}
		t.Fatalf("second open: err %v, want ErrLocked", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	openTest(t, dir, FsyncNever) // lock released by Close
}
