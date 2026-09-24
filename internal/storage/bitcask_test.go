package storage

import (
	"bytes"
	"errors"
	"fmt"
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
