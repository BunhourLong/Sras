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

// TestOpenSyncsDir covers recovery of an empty file left by an interrupted create.
func TestOpenSyncsDir(t *testing.T) {
	for _, policy := range []FsyncPolicy{FsyncAlways, FsyncInterval, FsyncNever} {
		t.Run(string(policy), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(datafilePath(dir, 1), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			calls := 0
			b, err := open(Options{Dir: dir, Fsync: policy}, func(got string) error {
				if got != dir {
					t.Fatalf("sync dir %q, want %q", got, dir)
				}
				calls++
				return syncDir(got)
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { b.Close() })
			if calls != 1 {
				t.Fatalf("open: %d dir syncs, want 1", calls)
			}
			putAll(t, b, "key", "value")
			wantGet(t, b, "key", []byte("value"))
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			b = openTest(t, dir, policy)
			wantGet(t, b, "key", []byte("value"))
		})
	}
}

// TestOpenDirSyncFailure verifies that a failed Open releases its directory lock.
func TestOpenDirSyncFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(datafilePath(dir, 1), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("dir sync failed")
	b, err := open(Options{Dir: dir, Fsync: FsyncInterval}, func(string) error { return syncErr })
	if b != nil {
		t.Cleanup(func() { b.Close() })
		t.Fatal("open returned a store after dir sync failure")
	}
	if !errors.Is(err, syncErr) {
		t.Fatalf("open: err %v, want dir sync error", err)
	}
	b = openTest(t, dir, FsyncAlways)
	putAll(t, b, "key", "value")
	wantGet(t, b, "key", []byte("value"))
}

func TestCreateSyncsDir(t *testing.T) {
	for _, policy := range []FsyncPolicy{FsyncAlways, FsyncInterval, FsyncNever} {
		t.Run(string(policy), func(t *testing.T) {
			dir := t.TempDir()
			b := openTest(t, dir, policy)
			b.opts.MaxFileSize = 30
			calls := 0
			b.syncDir = func(got string) error {
				if got != dir {
					t.Fatalf("sync dir %q, want %q", got, dir)
				}
				calls++
				return syncDir(got)
			}

			putAll(t, b, "first", "1")
			if calls != 1 {
				t.Fatalf("first create: %d dir syncs, want 1", calls)
			}
			putAll(t, b, "second", "2")
			if calls != 2 {
				t.Fatalf("rotation: %d dir syncs, want 2", calls)
			}
		})
	}
}

func TestCreateDirSyncFailure(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir, FsyncAlways)
	syncErr := errors.New("dir sync failed")
	b.syncDir = func(string) error { return syncErr }
	if err := b.Put([]byte("failed"), []byte("1")); !errors.Is(err, syncErr) {
		t.Fatalf("put: err %v, want dir sync error", err)
	}
	wantGet(t, b, "failed", nil)
	if b.active != nil || len(b.files) != 0 || b.keydir.len() != 0 || b.nextID != 2 {
		t.Fatalf("failed create published state or reused ID: active=%v, files=%d, keys=%d, nextID=%d",
			b.active, len(b.files), b.keydir.len(), b.nextID)
	}
	if _, err := os.Stat(datafilePath(dir, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed file cleanup: err %v, want not exist", err)
	}

	b.syncDir = syncDir
	putAll(t, b, "second", "2")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, dir, FsyncAlways)
	wantGet(t, b, "second", []byte("2"))
	wantGet(t, b, "failed", nil)
	if b.keydir.len() != 1 {
		t.Fatalf("reopened keydir has %d keys, want 1", b.keydir.len())
	}
}

func TestRotateDirSyncFailure(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir, FsyncAlways)
	b.opts.MaxFileSize = 60
	putAll(t, b, "first", "1", "second", "2")
	old, oldSize, nextID := b.active, b.active.size, b.nextID
	syncErr := errors.New("dir sync failed")
	b.syncDir = func(string) error { return syncErr }
	if err := b.Put([]byte("failed"), []byte("3")); !errors.Is(err, syncErr) {
		t.Fatalf("put: err %v, want dir sync error", err)
	}
	wantGet(t, b, "first", []byte("1"))
	wantGet(t, b, "second", []byte("2"))
	wantGet(t, b, "failed", nil)
	if b.active != old || b.files[old.id] != old || len(b.files) != 1 || old.size != oldSize || b.nextID != nextID+1 {
		t.Fatalf("failed rotation changed active/files/size or reused ID: active=%v, files=%d, size=%d, nextID=%d",
			b.active, len(b.files), old.size, b.nextID)
	}
	if _, err := os.Stat(datafilePath(dir, nextID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed file cleanup: err %v, want not exist", err)
	}

	b.syncDir = syncDir
	putAll(t, b, "third", "3")
	if b.active.id != nextID+1 {
		t.Fatalf("active file %d, want %d", b.active.id, nextID+1)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, dir, FsyncAlways)
	wantGet(t, b, "first", []byte("1"))
	wantGet(t, b, "second", []byte("2"))
	wantGet(t, b, "third", []byte("3"))
	wantGet(t, b, "failed", nil)
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

// scanKeys collects the keys Scan yields for prefix, stopping after limit
// (0 = no limit), and checks each value against Get.
func scanKeys(t *testing.T, b *Bitcask, prefix string, limit int) []string {
	t.Helper()
	var got []string
	err := b.Scan([]byte(prefix), func(k, v []byte) bool {
		wantGet(t, b, string(k), v)
		got = append(got, string(k))
		return limit == 0 || len(got) < limit
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestBitcaskScan(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir, FsyncNever)
	putAll(t, b, "users/2", "b", "users/10", "c", "user/1", "x", "users/1", "a", "orders/1", "o", "users/3", "gone")
	if err := b.Delete([]byte("users/3")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, prefix string
		limit        int
		want         []string
	}{
		{"collection", "users/", 0, []string{"users/1", "users/10", "users/2"}},
		{"shorter prefix", "user", 0, []string{"user/1", "users/1", "users/10", "users/2"}},
		{"all", "", 0, []string{"orders/1", "user/1", "users/1", "users/10", "users/2"}},
		{"no match", "nope/", 0, nil},
		{"early stop", "users/", 2, []string{"users/1", "users/10"}},
	}
	check := func(b *Bitcask) {
		t.Helper()
		for _, tt := range tests {
			if got := scanKeys(t, b, tt.prefix, tt.limit); fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("%s: scan %q = %v, want %v", tt.name, tt.prefix, got, tt.want)
			}
		}
	}
	check(b)

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Scan(nil, func(k, v []byte) bool { return true }); !errors.Is(err, ErrClosed) {
		t.Fatalf("scan after close: err %v, want ErrClosed", err)
	}
	check(openTest(t, dir, FsyncNever))
}

// fn may write (no lock is held while it runs), and a key deleted after the
// snapshot is skipped.
func TestBitcaskScanCallbackWrites(t *testing.T) {
	b := openTest(t, t.TempDir(), FsyncNever)
	putAll(t, b, "k/1", "a", "k/2", "b", "k/3", "c")

	var got []string
	err := b.Scan([]byte("k/"), func(k, v []byte) bool {
		got = append(got, string(k))
		if string(k) == "k/1" {
			if err := b.Delete([]byte("k/2")); err != nil {
				t.Fatal(err)
			}
			if err := b.Put([]byte("k/3"), []byte("new")); err != nil {
				t.Fatal(err)
			}
		}
		if string(k) == "k/3" && string(v) != "new" {
			t.Fatalf("k/3 = %q, want the newer value", v)
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[k/1 k/3]" {
		t.Fatalf("scan = %v, want [k/1 k/3]", got)
	}
}

func TestBitcaskConcurrentScan(t *testing.T) {
	b, err := Open(Options{Dir: t.TempDir(), Fsync: FsyncNever, MaxFileSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	for i := 0; i < 50; i++ {
		putAll(t, b, fmt.Sprintf("k/%03d", i), "v")
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // writer: overwrite, delete, add, rotating files as it goes
		defer wg.Done()
		for i := 0; i < 200; i++ {
			key := fmt.Appendf(nil, "k/%03d", i%50)
			if err := b.Put(key, []byte("w")); err != nil {
				t.Error(err)
				return
			}
			if i%7 == 0 {
				b.Delete(key)
			}
		}
	}()
	go func() { // scanner: every key is yielded once, in order
		defer wg.Done()
		for i := 0; i < 20; i++ {
			prev := ""
			err := b.Scan([]byte("k/"), func(k, v []byte) bool {
				if string(k) <= prev {
					t.Errorf("scan out of order: %q after %q", k, prev)
					return false
				}
				prev = string(k)
				return true
			})
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
}
