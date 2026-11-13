// Package enginetest is a conformance suite that any storage.Engine
// implementation must pass. A test calls RunEngineTests with an open function
// that starts an engine on a data directory; each behaviour runs as a subtest
// in its own temporary directory, and every engine the suite opens is closed
// again.
package enginetest

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"sras/internal/storage"
)

// RunEngineTests runs the Engine conformance suite against engines made by
// open. open gets a fresh temporary directory and must report failure through
// t itself (t.Fatal on error); it may be called again on the same directory
// once the previous engine there was closed. Every engine the suite opens is
// closed before its subtest ends, so double Close (an explicit one plus the
// suite's cleanup) must be tolerated; Bitcask's Close is idempotent.
func RunEngineTests(t *testing.T, open func(t *testing.T, dir string) storage.Engine) {
	t.Helper()

	t.Run("get missing", func(t *testing.T) {
		e := openEngine(t, open, t.TempDir())
		wantMissing(t, e, "missing")
	})

	t.Run("put get round trip", func(t *testing.T) {
		e := openEngine(t, open, t.TempDir())
		docs := []struct {
			key, val []byte
		}{
			{[]byte("users/1"), []byte(`{"a":1}`)},
			// An empty value: Get must return length 0 and a nil error.
			{[]byte("empty"), []byte{}},
			// Keys and values are opaque bytes.
			{[]byte{0x00, 0xff, '/', 'k'}, []byte("binary \x00\xff value")},
		}
		for _, d := range docs {
			put(t, e, string(d.key), d.val)
		}
		for _, d := range docs {
			wantValue(t, e, string(d.key), d.val)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		e := openEngine(t, open, t.TempDir())
		put(t, e, "k", []byte("first"))
		wantValue(t, e, "k", []byte("first"))
		put(t, e, "k", []byte("second"))
		wantValue(t, e, "k", []byte("second"))
	})

	t.Run("delete", func(t *testing.T) {
		e := openEngine(t, open, t.TempDir())
		put(t, e, "gone", []byte("v"))
		del(t, e, "gone")
		wantMissing(t, e, "gone")
		// The store is not empty here, so a Delete of a missing key must fail
		// without disturbing the live data around it.
		put(t, e, "sentinel", []byte("s"))
		if err := e.Delete([]byte("never-written")); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("delete never-written: err %v, want ErrNotFound", err)
		}
		if err := e.Delete([]byte("gone")); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("delete of an already-deleted key: err %v, want ErrNotFound", err)
		}
		wantValue(t, e, "sentinel", []byte("s"))
	})

	t.Run("scan prefix", func(t *testing.T) {
		e := openEngine(t, open, t.TempDir())
		vals := map[string]string{
			"a/1": "one", "a/2": "two", "a/3": "three",
			"ab/1": "ab", "b/1": "bee", "a/4": "dead",
		}
		// Insert unsorted; a/4 is deleted before scanning.
		for _, k := range []string{"a/3", "a/1", "ab/1", "b/1", "a/2", "a/4"} {
			put(t, e, k, []byte(vals[k]))
		}
		del(t, e, "a/4")

		scan := func(prefix string, stopAfterFirst bool) []string {
			t.Helper()
			var got []string
			err := e.Scan([]byte(prefix), func(k, v []byte) bool {
				got = append(got, string(k))
				if want := vals[string(k)]; !bytes.Equal(v, []byte(want)) {
					t.Errorf("scan %q: key %q has value %q, want %q", prefix, k, v, want)
				}
				// Returning false on the first call stops after one key.
				return !stopAfterFirst
			})
			if err != nil {
				t.Fatalf("scan %q: %v", prefix, err)
			}
			return got
		}

		if got := scan("a/", false); fmt.Sprint(got) != "[a/1 a/2 a/3]" {
			t.Fatalf("scan a/ = %v, want [a/1 a/2 a/3] in ascending order", got)
		}
		if got := scan("", false); fmt.Sprint(got) != "[a/1 a/2 a/3 ab/1 b/1]" {
			t.Fatalf("scan all = %v, want every live key sorted, without a/4", got)
		}
		if got := scan("a/", true); len(got) != 1 || got[0] != "a/1" {
			t.Fatalf("early stop visited %v, want exactly [a/1]", got)
		}
	})

	t.Run("scan callback writes", func(t *testing.T) {
		e := openEngine(t, open, t.TempDir())
		put(t, e, "k/1", []byte("a"))
		put(t, e, "k/2", []byte("b"))
		put(t, e, "k/3", []byte("c"))

		var first string
		var visited []string
		err := e.Scan([]byte("k/"), func(k, v []byte) bool {
			visited = append(visited, string(k))
			if len(visited) == 1 {
				first = string(k)
				// Writes from inside fn must not deadlock the scan.
				if err := e.Put([]byte("k/new"), []byte("n")); err != nil {
					t.Error(err)
					return false
				}
				if err := e.Delete(k); err != nil {
					t.Error(err)
					return false
				}
			}
			return true
		})
		if err != nil {
			t.Fatalf("scan with writes in fn: %v", err)
		}
		if len(visited) == 0 {
			t.Fatal("scan never called fn")
		}
		// The keys fn did not touch must all have been visited, so a scan that
		// stops after the first callback fails here. k/new may also show up:
		// whether an insert made mid-scan is visible is engine-defined.
		for _, want := range []string{"k/2", "k/3"} {
			seen := false
			for _, k := range visited {
				if k == want {
					seen = true
					break
				}
			}
			if !seen {
				t.Errorf("scan with writes in fn: %q was never visited, visited %v", want, visited)
			}
		}
		// The scan reflects the writes made inside it.
		wantMissing(t, e, first)
		wantValue(t, e, "k/2", []byte("b"))
		wantValue(t, e, "k/3", []byte("c"))
		wantValue(t, e, "k/new", []byte("n"))
	})

	// Data preservation only: space reclamation is engine-specific and tested
	// per engine (Bitcask: TestMerge in compaction_test.go).
	t.Run("merge keeps live data", func(t *testing.T) {
		dir := t.TempDir()
		e := openEngine(t, open, dir)

		// Overwritten live keys and deleted keys leave plenty of dead records
		// to compact; the 256-byte values make the engine rotate files.
		const live, dead = 30, 10
		for i := 0; i < live; i++ {
			key := fmt.Sprintf("l/%02d", i)
			put(t, e, key, bigValue("first", i))
			put(t, e, key, bigValue("second", i))
		}
		for i := 0; i < dead; i++ {
			key := fmt.Sprintf("d/%02d", i)
			put(t, e, key, bigValue("dead", i))
			del(t, e, key)
		}

		check := func() {
			t.Helper()
			for i := 0; i < live; i++ {
				wantValue(t, e, fmt.Sprintf("l/%02d", i), bigValue("second", i))
			}
			for i := 0; i < dead; i++ {
				wantMissing(t, e, fmt.Sprintf("d/%02d", i))
			}
		}
		if err := e.Merge(); err != nil {
			t.Fatalf("merge: %v", err)
		}
		check()
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}

		e = openEngine(t, open, dir)
		check()
	})

	t.Run("reopen", func(t *testing.T) {
		dir := t.TempDir()
		e := openEngine(t, open, dir)

		const live, dead = 10, 5
		for i := 0; i < live; i++ {
			key := fmt.Sprintf("r/%02d", i)
			put(t, e, key, bigValue("first", i))
			put(t, e, key, bigValue("second", i))
		}
		for i := 0; i < dead; i++ {
			key := fmt.Sprintf("x/%02d", i)
			put(t, e, key, bigValue("dead", i))
			del(t, e, key)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}

		e = openEngine(t, open, dir)
		for i := 0; i < live; i++ {
			wantValue(t, e, fmt.Sprintf("r/%02d", i), bigValue("second", i))
		}
		for i := 0; i < dead; i++ {
			wantMissing(t, e, fmt.Sprintf("x/%02d", i))
		}
		// A fresh write after reopening works and reads back.
		put(t, e, "r/new", bigValue("after reopen", 0))
		wantValue(t, e, "r/new", bigValue("after reopen", 0))
	})

	t.Run("after close", func(t *testing.T) {
		e := openEngine(t, open, t.TempDir())
		put(t, e, "k", []byte("v"))
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Get([]byte("k")); !errors.Is(err, storage.ErrClosed) {
			t.Fatalf("get after close: err %v, want ErrClosed", err)
		}
		if err := e.Put([]byte("k"), []byte("v2")); !errors.Is(err, storage.ErrClosed) {
			t.Fatalf("put after close: err %v, want ErrClosed", err)
		}
		if err := e.Delete([]byte("k")); !errors.Is(err, storage.ErrClosed) {
			t.Fatalf("delete after close: err %v, want ErrClosed", err)
		}
	})

	t.Run("concurrent put get", func(t *testing.T) {
		e := openEngine(t, open, t.TempDir())
		shared := []byte("shared value")
		put(t, e, "shared", shared)

		const workers, perWorker = 8, 200
		var writers, readers sync.WaitGroup
		done := make(chan struct{})

		// Readers hit one shared key while the writers run. t.Errorf only:
		// t.Fatal must not be called from a non-test goroutine.
		for r := 0; r < 2; r++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for {
					select {
					case <-done:
						return
					default:
					}
					got, err := e.Get([]byte("shared"))
					if err != nil || !bytes.Equal(got, shared) {
						t.Errorf("get shared = %q, %v", got, err)
						return
					}
				}
			}()
		}
		for w := 0; w < workers; w++ {
			writers.Add(1)
			go func(w int) {
				defer writers.Done()
				for n := 0; n < perWorker; n++ {
					key := fmt.Appendf(nil, "g%d/%d", w, n)
					val := fmt.Appendf(nil, "v%d/%d", w, n)
					if err := e.Put(key, val); err != nil {
						t.Errorf("put %s: %v", key, err)
						return
					}
					got, err := e.Get(key)
					if err != nil || !bytes.Equal(got, val) {
						t.Errorf("get %s = %q, %v", key, got, err)
						return
					}
				}
			}(w)
		}
		writers.Wait()
		close(done)
		readers.Wait()
	})
}

// openEngine opens an engine on dir and closes it again when the subtest ends.
// Reopening a directory needs the previous engine closed first, which an
// explicit Close earlier in the test also satisfies.
func openEngine(t *testing.T, open func(t *testing.T, dir string) storage.Engine, dir string) storage.Engine {
	t.Helper()
	e := open(t, dir)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// put stores key=val or fails the test.
func put(t *testing.T, e storage.Engine, key string, val []byte) {
	t.Helper()
	if err := e.Put([]byte(key), val); err != nil {
		t.Fatalf("put %q: %v", key, err)
	}
}

// del removes key or fails the test.
func del(t *testing.T, e storage.Engine, key string) {
	t.Helper()
	if err := e.Delete([]byte(key)); err != nil {
		t.Fatalf("delete %q: %v", key, err)
	}
}

// wantValue checks Get(key) returns exactly val.
func wantValue(t *testing.T, e storage.Engine, key string, val []byte) {
	t.Helper()
	got, err := e.Get([]byte(key))
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	if !bytes.Equal(got, val) {
		t.Fatalf("get %q = %q, want %q", key, got, val)
	}
}

// wantMissing checks Get(key) returns ErrNotFound.
func wantMissing(t *testing.T, e storage.Engine, key string) {
	t.Helper()
	if _, err := e.Get([]byte(key)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("get %q: err %v, want ErrNotFound", key, err)
	}
}

// bigValue returns a 256-byte value embedding tag and n, so the versions of a
// key differ on disk and the engine rotates through several files even with a
// small MaxFileSize.
func bigValue(tag string, n int) []byte {
	head := fmt.Appendf(nil, "%s/%04d/", tag, n)
	return append(head, bytes.Repeat([]byte{'x'}, 256-len(head))...)
}
