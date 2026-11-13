package storage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func openSmall(t *testing.T, dir string) *Bitcask {
	t.Helper()
	b, err := Open(Options{Dir: dir, Fsync: FsyncNever, MaxFileSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// dataFiles returns the size of every .data file in dir, by name.
func dataFiles(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*"+datafileExt))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int64, len(paths))
	for _, p := range paths {
		out[filepath.Base(p)] = fileSize(t, p)
	}
	return out
}

func diskUsage(t *testing.T, dir string) (total int64) {
	for _, n := range dataFiles(t, dir) {
		total += n
	}
	return total
}

// fillForMerge overwrites 10 keys 10 times across many rotated files, deletes
// k/0..k/2, and returns the expected contents.
func fillForMerge(t *testing.T, b *Bitcask) map[string][]byte {
	t.Helper()
	want := map[string][]byte{}
	for round := 0; round < 10; round++ {
		for i := 0; i < 10; i++ {
			k, v := fmt.Sprintf("k/%d", i), fmt.Sprintf("v%d-%d", i, round)
			putAll(t, b, k, v)
			want[k] = []byte(v)
		}
	}
	for i := 0; i < 3; i++ {
		k := fmt.Sprintf("k/%d", i)
		if err := b.Delete([]byte(k)); err != nil {
			t.Fatal(err)
		}
		want[k] = nil
	}
	return want
}

func checkWant(t *testing.T, b *Bitcask, want map[string][]byte) {
	t.Helper()
	for k, v := range want {
		wantGet(t, b, k, v)
	}
}

func TestMerge(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	want := fillForMerge(t, b)
	before := diskUsage(t, dir)

	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	checkWant(t, b, want)
	if after := diskUsage(t, dir); after >= before/2 {
		t.Fatalf("disk usage %d -> %d, want it to shrink a lot", before, after)
	}
	for id, df := range b.files {
		if df.readOnly != (df != b.active) {
			t.Fatalf("file %d readOnly=%v, active=%d", id, df.readOnly, b.active.id)
		}
		if id > b.active.id {
			t.Fatalf("file %d is above the active file %d", id, b.active.id)
		}
	}

	// Writes after the merge, a second merge over merged files, then reopen.
	putAll(t, b, "k/5", "after")
	want["k/5"] = []byte("after")
	if err := b.Delete([]byte("k/6")); err != nil {
		t.Fatal(err)
	}
	want["k/6"] = nil
	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	checkWant(t, b, want)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge(); !errors.Is(err, ErrClosed) {
		t.Fatalf("merge after close: %v, want ErrClosed", err)
	}

	b = openSmall(t, dir)
	checkWant(t, b, want)
	if n := b.keydir.len(); n != 6 {
		t.Fatalf("keydir has %d keys after reopen, want 6", n)
	}
}

func TestMergeNothingToDo(t *testing.T) {
	b := openSmall(t, t.TempDir())
	if err := b.Merge(); err != nil { // no files at all
		t.Fatal(err)
	}
	putAll(t, b, "a", "1")
	if err := b.Merge(); err != nil { // only the active file
		t.Fatal(err)
	}
	wantGet(t, b, "a", []byte("1"))
}

// A crash after the outputs are renamed but before (or while) the old files
// are deleted must still replay correctly. Deletion goes in ascending ID order,
// so the old files left behind are always a suffix of the snapshot.
func TestMergeCrashBeforeDelete(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	want := fillForMerge(t, b)

	b.fmu.RLock()
	var oldNames []string
	for _, df := range b.files {
		if df != b.active {
			oldNames = append(oldNames, filepath.Base(df.path))
		}
	}
	b.fmu.RUnlock()
	slices.Sort(oldNames)
	saved := map[string][]byte{}
	for _, n := range oldNames {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		saved[n] = data
	}
	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	merged := map[string][]byte{}
	for name := range dataFiles(t, dir) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		merged[name] = data
	}

	// Leave behind the old files from each cut point on.
	for cut := range oldNames {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			crashed := t.TempDir()
			write := func(name string, data []byte) {
				if err := os.WriteFile(filepath.Join(crashed, name), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for name, data := range merged {
				write(name, data)
			}
			for _, name := range oldNames[cut:] {
				write(name, saved[name])
			}
			// An unfinished output from a later merge is thrown away.
			write("000999.data"+tmpExt, []byte("garbage"))
			tmp := filepath.Join(crashed, "000999.data"+tmpExt)
			b := openSmall(t, crashed)
			checkWant(t, b, want)
			if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temp file survived Open: %v", err)
			}
		})
	}
}

// Regression: a key copied by Merge and then deleted before the swap has its
// tombstone in a file below the output. Without a fresh tombstone above the
// output, reopening resurrected the key.
func TestMergeWritesDuringMerge(t *testing.T) {
	dir := t.TempDir()
	b := openSmall(t, dir)
	putAll(t, b, "del", "1", "put", "1", "keep", "1")
	b.wmu.Lock()
	_, err := b.rotate(b.active)
	b.wmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	b.fmu.RLock()
	olds := []*datafile{b.files[1]}
	b.fmu.RUnlock()

	b.mergeMu.Lock()
	outs, moves, err := b.copyLive(olds)
	if err != nil {
		t.Fatal(err)
	}
	// Writes land between the copy and the swap.
	if err := b.Delete([]byte("del")); err != nil {
		t.Fatal(err)
	}
	putAll(t, b, "put", "2")
	if err := b.installMerge(olds, outs, moves); err != nil {
		t.Fatal(err)
	}
	b.mergeMu.Unlock()

	want := map[string][]byte{"del": nil, "put": []byte("2"), "keep": []byte("1")}
	checkWant(t, b, want)
	// The old file is off the map but still on disk, as after a crash.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = openSmall(t, dir)
	checkWant(t, b, want)
	if err := b.Merge(); err != nil {
		t.Fatal(err)
	}
	checkWant(t, b, want)
}

func TestMergeConcurrent(t *testing.T) {
	b := openSmall(t, t.TempDir())
	const workers, perWorker = 4, 200
	stop := make(chan struct{})
	mergeDone := make(chan error)
	go func() {
		for {
			select {
			case <-stop:
				mergeDone <- nil
				return
			default:
			}
			if err := b.Merge(); err != nil {
				mergeDone <- err
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				k := fmt.Appendf(nil, "k/%d/%d", w, i%10)
				v := fmt.Appendf(nil, "v%d", i)
				if err := b.Put(k, v); err != nil {
					t.Error(err)
					return
				}
				if got, err := b.Get(k); err != nil || !bytes.Equal(got, v) {
					t.Errorf("get %s = %q, %v; want %q", k, got, err, v)
					return
				}
				if i%7 == 0 {
					if err := b.Delete(k); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	if err := <-mergeDone; err != nil {
		t.Fatal(err)
	}
}
