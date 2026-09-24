package storage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T, dir string, fsync FsyncPolicy) *Bitcask {
	t.Helper()
	b, err := Open(Options{Dir: dir, Fsync: fsync, SyncInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func TestBitcaskPutGet(t *testing.T) {
	for _, policy := range []FsyncPolicy{FsyncAlways, FsyncInterval, FsyncNever} {
		t.Run(string(policy), func(t *testing.T) {
			b := openTest(t, t.TempDir(), policy)

			if _, err := b.Get([]byte("k")); !errors.Is(err, ErrNotFound) {
				t.Fatalf("get missing: err %v, want ErrNotFound", err)
			}
			steps := []struct{ key, val string }{
				{"users/1", `{"a":1}`},
				{"users/2", `{"b":2}`},
				{"users/1", `{"a":"overwritten"}`},
				{"users/3", ``},
			}
			for _, s := range steps {
				if err := b.Put([]byte(s.key), []byte(s.val)); err != nil {
					t.Fatal(err)
				}
			}
			want := map[string]string{"users/1": `{"a":"overwritten"}`, "users/2": `{"b":2}`, "users/3": ``}
			for k, v := range want {
				got, err := b.Get([]byte(k))
				if err != nil {
					t.Fatalf("get %s: %v", k, err)
				}
				if !bytes.Equal(got, []byte(v)) {
					t.Fatalf("get %s = %q, want %q", k, got, v)
				}
			}

			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if err := b.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
				t.Fatalf("put after close: err %v, want ErrClosed", err)
			}
		})
	}
}

// The newest file must reopen writable, and appends must go after what is
// already on disk.
func TestBitcaskReopenAppends(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir, FsyncNever)
	if err := b.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	size := b.active.size
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b = openTest(t, dir, FsyncNever)
	if err := b.Put([]byte("b"), []byte("2")); err != nil {
		t.Fatal(err)
	}
	if b.active.id != 1 || b.active.size <= size {
		t.Fatalf("active file %d size %d, want file 1 grown past %d", b.active.id, b.active.size, size)
	}
	if got, err := b.Get([]byte("b")); err != nil || string(got) != "2" {
		t.Fatalf("get b = %q, %v", got, err)
	}
}

func TestBitcaskConcurrentPutGet(t *testing.T) {
	b := openTest(t, t.TempDir(), FsyncInterval)
	const workers, perWorker = 8, 100

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := fmt.Appendf(nil, "k/%d/%d", w, i)
				val := fmt.Appendf(nil, "v%d", i)
				if err := b.Put(key, val); err != nil {
					t.Error(err)
					return
				}
				got, err := b.Get(key)
				if err != nil || !bytes.Equal(got, val) {
					t.Errorf("get %s = %q, %v", key, got, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if n := b.keydir.len(); n != workers*perWorker {
		t.Fatalf("keydir has %d keys, want %d", n, workers*perWorker)
	}
}

func TestBitcaskRotation(t *testing.T) {
	dir := t.TempDir()
	const maxSize = 100
	open := func() *Bitcask {
		b, err := Open(Options{Dir: dir, Fsync: FsyncNever, MaxFileSize: maxSize})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		return b
	}
	b := open()
	const n = 50
	for i := 0; i < n; i++ {
		putAll(t, b, fmt.Sprintf("k/%02d", i), fmt.Sprintf("value-%02d", i))
	}
	// One value bigger than MaxFileSize gets a file of its own.
	big := bytes.Repeat([]byte("x"), 3*maxSize)
	putAll(t, b, "big", string(big))
	bigFile := b.active.id
	putAll(t, b, "after", "1")

	checkAll := func(b *Bitcask) {
		t.Helper()
		for i := 0; i < n; i++ {
			wantGet(t, b, fmt.Sprintf("k/%02d", i), fmt.Appendf(nil, "value-%02d", i))
		}
		wantGet(t, b, "big", big)
		wantGet(t, b, "after", []byte("1"))
	}
	checkAll(b)

	if len(b.files) < 3 {
		t.Fatalf("%d data files, want several", len(b.files))
	}
	for id, df := range b.files {
		if df.readOnly != (df != b.active) {
			t.Fatalf("file %d readOnly=%v, active=%d", id, df.readOnly, b.active.id)
		}
		if id != bigFile && df.size > maxSize {
			t.Fatalf("file %d is %d bytes, max %d", id, df.size, maxSize)
		}
	}
	if b.active.id == bigFile {
		t.Fatalf("put after the oversized value went into its file %d", bigFile)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Recovery replays every file and keeps appending to the newest.
	b = open()
	checkAll(b)
	putAll(t, b, "k/00", "new")
	wantGet(t, b, "k/00", []byte("new"))
}

// After a real rotation, damage in an immutable file must stop Open.
func TestBitcaskRotatedFileCorrupt(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(Options{Dir: dir, Fsync: FsyncNever, MaxFileSize: 60})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		putAll(t, b, fmt.Sprintf("k/%d", i), "some value")
	}
	if b.active.id < 2 {
		t.Fatalf("active file %d, want a rotation", b.active.id)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	path := datafilePath(dir, 1)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := Open(Options{Dir: dir}); !errors.Is(err, ErrCorrupt) {
		if err == nil {
			b.Close()
		}
		t.Fatalf("open: err %v, want ErrCorrupt", err)
	}
}

// Readers and the interval syncer must keep working while Puts rotate files.
func TestBitcaskConcurrentRotation(t *testing.T) {
	b, err := Open(Options{Dir: t.TempDir(), Fsync: FsyncInterval, SyncInterval: time.Millisecond, MaxFileSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	const workers, perWorker = 8, 100

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := fmt.Appendf(nil, "k/%d/%d", w, i)
				val := fmt.Appendf(nil, "v%d", i)
				if err := b.Put(key, val); err != nil {
					t.Error(err)
					return
				}
				// Re-read an earlier key, which likely lives in a rotated file.
				old := fmt.Appendf(nil, "k/%d/%d", w, i/2)
				if got, err := b.Get(old); err != nil || !bytes.Equal(got, fmt.Appendf(nil, "v%d", i/2)) {
					t.Errorf("get %s = %q, %v", old, got, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if len(b.files) < 10 {
		t.Fatalf("%d data files, want many rotations", len(b.files))
	}
}

func TestBitcaskDelete(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir, FsyncNever)

	if err := b.Delete([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: err %v, want ErrNotFound", err)
	}
	if b.active != nil {
		t.Fatal("delete of a missing key wrote a record")
	}

	putAll(t, b, "gone", "1", "back", "old", "kept", "k")
	for _, k := range []string{"gone", "back"} {
		if err := b.Delete([]byte(k)); err != nil {
			t.Fatal(err)
		}
		wantGet(t, b, k, nil)
	}
	if err := b.Delete([]byte("gone")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: err %v, want ErrNotFound", err)
	}
	putAll(t, b, "back", "new")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete([]byte("kept")); !errors.Is(err, ErrClosed) {
		t.Fatalf("delete after close: err %v, want ErrClosed", err)
	}

	// Tombstones survive a restart; a Put after a Delete wins.
	b = openTest(t, dir, FsyncNever)
	wantGet(t, b, "gone", nil)
	wantGet(t, b, "back", []byte("new"))
	wantGet(t, b, "kept", []byte("k"))
	if n := b.keydir.len(); n != 2 {
		t.Fatalf("keydir has %d keys, want 2", n)
	}
}

// A tombstone that lands in a newer file than the value still hides it after
// a restart.
func TestBitcaskDeleteAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(Options{Dir: dir, Fsync: FsyncNever, MaxFileSize: 60})
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, b, "a", "value in file 1")
	putAll(t, b, "filler", "pushes the tombstone into a newer file")
	if err := b.Delete([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if b.active.id == 1 {
		t.Fatal("tombstone is in file 1, want a newer file")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b = openTest(t, dir, FsyncNever)
	wantGet(t, b, "a", nil)
}
