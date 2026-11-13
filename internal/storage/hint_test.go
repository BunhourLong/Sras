package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func installTestHint(t *testing.T, dir string, id uint32, size int64, entries []hintEntry) {
	t.Helper()
	if err := writeHint(dir, id, size, entries); err != nil {
		t.Fatal(err)
	}
	path := hintPath(dir, id)
	if err := os.Rename(path+tmpExt, path); err != nil {
		t.Fatal(err)
	}
}

func fixHintCRC(buf []byte) []byte {
	end := len(buf) - crcSize
	binary.LittleEndian.PutUint32(buf[end:], crc32.ChecksumIEEE(buf[:end]))
	return buf
}

func TestHintRoundTripAndValidation(t *testing.T) {
	dir := t.TempDir()
	const id = 7
	const size = 52
	want := []hintEntry{
		{key: "alpha", entry: entry{fileID: id, valueOff: 25, valueSize: 3, timestamp: -1}},
		{key: "beta", entry: entry{fileID: id, valueOff: 52, valueSize: 0, timestamp: math.MaxInt64}},
	}
	installTestHint(t, dir, id, size, want)
	got, err := readHint(dir, id, size)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("readHint = %v, %v; want %v", got, err, want)
	}
	path := hintPath(dir, id)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// An empty output is handled without indexing into a nonexistent move.
	installTestHint(t, dir, 8, 0, nil)
	if got, err := readHint(dir, 8, 0); err != nil || len(got) != 0 {
		t.Fatalf("empty hint = %v, %v", got, err)
	}
	if _, err := readHint(dir, 9, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing hint: %v", err)
	}

	const first = hintHeaderSize
	cases := []struct {
		name   string
		mutate func([]byte) []byte
		size   int64
	}{
		{"flipped byte", func(b []byte) []byte { b[len(b)-5] ^= 1; return b }, size},
		{"short header", func(b []byte) []byte { return b[:hintHeaderSize-1] }, size},
		{"missing trailer", func(b []byte) []byte { return b[:len(b)-crcSize] }, size},
		{"wrong magic", func(b []byte) []byte { b[0] = 'X'; return fixHintCRC(b) }, size},
		{"wrong version", func(b []byte) []byte { b[4] = 2; return fixHintCRC(b) }, size},
		{"data size mismatch", func(b []byte) []byte { return b }, size + 1},
		{"negative data size", func(b []byte) []byte { return b }, -1},
		{"truncated at entry boundary", func(b []byte) []byte {
			return fixHintCRC(append(b[:first+hintEntrySize+len("alpha")], make([]byte, crcSize)...))
		}, size},
		{"short entry", func(b []byte) []byte {
			return fixHintCRC(append(b[:first+hintEntrySize-1], make([]byte, crcSize)...))
		}, size},
		{"short key", func(b []byte) []byte {
			return fixHintCRC(append(b[:len(b)-crcSize-1], make([]byte, crcSize)...))
		}, size},
		{"trailing byte", func(b []byte) []byte {
			return fixHintCRC(append(b[:len(b)-crcSize], make([]byte, crcSize+1)...))
		}, size},
		{"empty key", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[first+8:], 0)
			return fixHintCRC(b)
		}, size},
		{"huge key", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[first+8:], math.MaxUint32)
			return fixHintCRC(b)
		}, size},
		{"offset before key end", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[first+16:], 24)
			return fixHintCRC(b)
		}, size},
		{"negative offset", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[first+16:], math.MaxUint64)
			return fixHintCRC(b)
		}, size},
		{"offset past EOF", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[first+16:], size+1)
			return fixHintCRC(b)
		}, size},
		{"value past EOF", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[first+12:], size)
			return fixHintCRC(b)
		}, size},
		{"offset addition overflow", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[first+16:], math.MaxInt64)
			return fixHintCRC(b)
		}, size},
		{"gap between records", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[first+16:], 26)
			return fixHintCRC(b)
		}, size},
		{"tombstone size", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[first+12:], TombstoneValueSize)
			return fixHintCRC(b)
		}, size},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := tc.mutate(bytes.Clone(valid))
			if err := os.WriteFile(path, buf, 0o644); err != nil {
				t.Fatal(err)
			}
			if got, err := readHint(dir, id, tc.size); !errors.Is(err, ErrCorrupt) || got != nil {
				t.Fatalf("readHint = %v, %v; want no entries, ErrCorrupt", got, err)
			}
		})
	}
}

func hintFiles(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*"+hintExt))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func wantNoTemps(t *testing.T, dir string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*"+tmpExt))
	if err != nil || len(paths) != 0 {
		t.Fatalf("temporary files = %v, %v; want none", paths, err)
	}
}

func TestMergeHintsRecovery(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	b.opts.MaxFileSize = 100
	want := fillForMerge(t, b)
	previousActive := b.active.id
	firstOutput := b.nextID
	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	outputs := 0
	for id, df := range b.files {
		_, err := os.Stat(hintPath(dir, id))
		if id >= firstOutput && df != b.active {
			outputs++
			if err != nil {
				t.Fatalf("merge output %d has no hint: %v", id, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-merge file %d has a hint: %v", id, err)
		}
	}
	if outputs < 2 {
		t.Fatalf("want multiple merge outputs, got %d", outputs)
	}
	if _, err := os.Stat(hintPath(dir, previousActive)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous active has a hint: %v", err)
	}
	wantNoTemps(t, dir)
	wantIndex, wantTS := maps.Clone(b.keydir.entries), b.lastTS
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openSmall(t, dir)
	if !maps.Equal(b.keydir.entries, wantIndex) || b.lastTS != wantTS {
		t.Fatalf("hint recovery changed keydir or lastTS: %v, %d; want %v, %d", b.keydir.entries, b.lastTS, wantIndex, wantTS)
	}
	checkWant(t, b, want)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range hintFiles(t, dir) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	b = openSmall(t, dir)
	if !maps.Equal(b.keydir.entries, wantIndex) || b.lastTS != wantTS {
		t.Fatalf("scan recovery differs from hint recovery: %v, %d; want %v, %d", b.keydir.entries, b.lastTS, wantIndex, wantTS)
	}
	checkWant(t, b, want)
}

func TestRecoveryUsesHint(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	want := fillForMerge(t, b)
	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	var key string
	var e entry
	for k, candidate := range b.keydir.entries {
		if _, err := os.Stat(hintPath(dir, candidate.fileID)); err == nil {
			key, e = k, candidate
			break
		}
	}
	if key == "" || e.valueSize == 0 {
		t.Fatal("no hinted value found")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	path := datafilePath(dir, e.fileID)
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	buf[e.valueOff] ^= 1
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	b = openSmall(t, dir) // The immutable data file must not be scanned.
	if _, err := b.Get([]byte(key)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("damaged value Get: %v, want ErrCorrupt", err)
	}
	for k, v := range want {
		if k != key {
			wantGet(t, b, k, v)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hintPath(dir, e.fileID)); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(Options{Dir: dir, Fsync: FsyncNever}); !errors.Is(err, ErrCorrupt) {
		if err == nil {
			reopened.Close()
		}
		t.Fatalf("Open without hint: %v, want ErrCorrupt", err)
	}
}

func TestRecoveryBadHintFallsBack(t *testing.T) {
	for _, damage := range []string{"flip", "truncate", "unreadable", "missing"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			b := openSmall(t, dir)
			want := fillForMerge(t, b)
			if err := b.Merge(); err != nil {
				t.Fatal(err)
			}
			wantIndex, wantTS := maps.Clone(b.keydir.entries), b.lastTS
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			path := hintFiles(t, dir)[0]
			buf, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "flip":
				buf[len(buf)-5] ^= 1
			case "truncate":
				buf = buf[:len(buf)-5]
			case "unreadable", "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if damage == "unreadable" {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if damage != "missing" {
				if err := os.WriteFile(path, buf, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			b, err = Open(Options{Dir: dir, Fsync: FsyncNever, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { b.Close() })
			if !maps.Equal(b.keydir.entries, wantIndex) || b.lastTS != wantTS {
				t.Fatal("fallback changed keydir or lastTS")
			}
			checkWant(t, b, want)
			if damage == "missing" {
				if logs.Len() != 0 {
					t.Fatalf("missing hint should be silent: %s", &logs)
				}
			} else if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), path) {
				t.Fatalf("no hint warning logged: %s", &logs)
			}
		})
	}
}

func TestMergeHintCleanup(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	want := fillForMerge(t, b)
	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	oldHints := hintFiles(t, dir)
	putAll(t, b, "k/5", "after")
	want["k/5"] = []byte("after")
	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	for _, path := range oldHints {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old hint survived second merge: %s, %v", path, err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	temp := hintPath(dir, 999) + tmpExt
	orphan := hintPath(dir, 998)
	for _, path := range []string{temp, orphan} {
		if err := os.WriteFile(path, []byte("leftover"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b = openSmall(t, dir)
	checkWant(t, b, want)
	wantNoTemps(t, dir)
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan hint survived Open: %v", err)
	}
}

func TestMergeHintFailureCleanup(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	b.opts.MaxFileSize = 100
	want := fillForMerge(t, b)
	// The second output's hint cannot be created; the first hint and every
	// output data temp must still be removed, leaving the old files usable.
	if err := os.WriteFile(hintPath(dir, b.nextID+1)+tmpExt, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge(); err == nil {
		t.Fatal("expected hint write failure")
	}
	wantNoTemps(t, dir)
	checkWant(t, b, want)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openSmall(t, dir)
	checkWant(t, b, want)
}

func TestRecoveryIgnoresActiveHint(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	putAll(t, b, "real", "value")
	e, _ := b.keydir.get("real")
	id, size := b.active.id, b.active.size
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// Structurally valid, but deliberately names another key of the same size.
	installTestHint(t, dir, id, size, []hintEntry{{key: "fake", entry: e}})
	b = openSmall(t, dir)
	wantGet(t, b, "real", []byte("value"))
	wantGet(t, b, "fake", nil)
}

func TestRecoveryHintTimestamps(t *testing.T) {
	dir := t.TempDir()
	writeRecords(t, dir, 1, nil,
		Record{Timestamp: 30, Key: []byte("newer"), Value: []byte("first")},
		Record{Timestamp: 20, Key: []byte("tie"), Value: []byte("first")},
	)
	const future = int64(1) << 62
	records := []Record{
		{Timestamp: 29, Key: []byte("newer"), Value: []byte("stale")},
		{Timestamp: 20, Key: []byte("tie"), Value: []byte("second")},
		{Timestamp: future, Key: []byte("future"), Value: []byte("value")},
	}
	writeRecords(t, dir, 2, nil, records...)
	writeRecords(t, dir, 3, nil) // active; the highest timestamp exists only in the hint
	var entries []hintEntry
	var off int64
	for _, rec := range records {
		valueOff := off + headerSize + int64(len(rec.Key))
		entries = append(entries, hintEntry{key: string(rec.Key), entry: entry{
			fileID: 2, valueOff: valueOff, valueSize: uint32(len(rec.Value)), timestamp: rec.Timestamp,
		}})
		off = valueOff + int64(len(rec.Value))
	}
	installTestHint(t, dir, 2, off, entries)
	b := openSmall(t, dir)
	wantGet(t, b, "newer", []byte("first"))
	wantGet(t, b, "tie", []byte("second"))
	wantGet(t, b, "future", []byte("value"))
	if b.lastTS != future {
		t.Fatalf("lastTS = %d, want %d", b.lastTS, future)
	}
	wantIndex := maps.Clone(b.keydir.entries)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hintPath(dir, 2)); err != nil {
		t.Fatal(err)
	}
	b = openSmall(t, dir)
	if !maps.Equal(b.keydir.entries, wantIndex) || b.lastTS != future {
		t.Fatal("hint timestamp replay differs from scan")
	}
}

// Regression: a merge that failed after renaming an output to .data left it on
// disk but outside the engine, so a later merge dropped the key's tombstone and
// reopen resurrected the key from the stray output.
func TestMergeRenameFailureNoResurrect(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	big := strings.Repeat("v", 150)
	putAll(t, b, "victim", big, "pad1", big) // pad1 rotates victim's file to immutable

	// Block the output's hint rename: its data rename has already succeeded.
	block := hintPath(dir, b.nextID)
	if err := os.MkdirAll(filepath.Join(block, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge(); err == nil {
		t.Fatal("expected hint rename failure")
	}
	if err := os.RemoveAll(block); err != nil {
		t.Fatal(err)
	}

	if err := b.Delete([]byte("victim")); err != nil {
		t.Fatal(err)
	}
	putAll(t, b, "pad2", big, "pad3", big) // rotate the tombstone into an immutable file
	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openSmall(t, dir)
	wantGet(t, b, "victim", nil)
	wantGet(t, b, "pad2", []byte(big))
}
