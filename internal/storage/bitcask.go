package storage

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// lockFileName is the file in the data dir that Open holds an exclusive flock
// on, so a second process (or a second Open) can't use the same dir.
const lockFileName = "LOCK"

var _ Engine = (*Bitcask)(nil)

// Bitcask is an append-only log store with an in-memory hash index. There is a
// single writer (wmu); readers run concurrently against the immutable files.
type Bitcask struct {
	opts Options
	log  *slog.Logger

	wmu    sync.Mutex // serialises writers
	lastTS int64      // newest record timestamp; guarded by wmu (set by Open before any Put)

	fmu    sync.RWMutex // guards the fields below
	files  map[uint32]*datafile
	active *datafile
	nextID uint32
	closed bool

	keydir *keydir

	lock *os.File // holds the flock on the data dir's LOCK file

	stopSync chan struct{} // closed by Close to stop the interval syncer
	syncDone chan struct{} // closed when the interval syncer exits
}

// Open loads the data dir and rebuilds the keydir.
func Open(opts Options) (*Bitcask, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("storage: Options.Dir is required")
	}
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = DefaultMaxFileSize
	}
	if opts.Fsync == "" {
		opts.Fsync = FsyncInterval
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = DefaultSyncInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create data dir: %w", err)
	}

	lock, err := lockDir(opts.Dir)
	if err != nil {
		return nil, err
	}

	ids, err := listDatafileIDs(opts.Dir)
	if err != nil {
		lock.Close()
		return nil, err
	}

	b := &Bitcask{
		opts:   opts,
		log:    opts.Logger,
		files:  make(map[uint32]*datafile, len(ids)),
		nextID: 1,
		keydir: newKeydir(),
		lock:   lock,
	}
	for i, id := range ids {
		// Only the newest file is written to; the rest are immutable.
		df, err := openDatafile(opts.Dir, id, i < len(ids)-1)
		if err != nil {
			b.Close()
			return nil, fmt.Errorf("storage: %w", err)
		}
		b.files[id] = df
		b.nextID = id + 1
	}
	// The highest-numbered file is the active one; a fresh dir has none until
	// the first write creates it.
	if len(ids) > 0 {
		b.active = b.files[ids[len(ids)-1]]
	}

	if err := b.rebuildKeydir(ids); err != nil {
		b.Close()
		return nil, fmt.Errorf("storage: recovery: %w", err)
	}

	if opts.Fsync == FsyncInterval {
		b.stopSync = make(chan struct{})
		b.syncDone = make(chan struct{})
		go b.syncLoop()
	}
	return b, nil
}

// lockDir takes an exclusive, non-blocking flock on dir/LOCK. The OS drops the
// lock when the process dies, so a crash never leaves a stale lock behind.
// flock is Unix-only (macOS/Linux).
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("storage: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("storage: %s: %w", dir, ErrLocked)
		}
		return nil, fmt.Errorf("storage: lock data dir: %w", err)
	}
	return f, nil
}

// syncLoop fsyncs the active file every SyncInterval until Close stops it.
func (b *Bitcask) syncLoop() {
	defer close(b.syncDone)
	t := time.NewTicker(b.opts.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-b.stopSync:
			return
		case <-t.C:
			// Hold fmu across the sync so rotation can't close the file under us.
			b.fmu.RLock()
			if b.active != nil {
				if err := b.active.sync(); err != nil {
					b.log.Error("storage: interval sync", "err", err)
				}
			}
			b.fmu.RUnlock()
		}
	}
}

// listDatafileIDs returns the IDs of the data files in dir, in ascending order.
func listDatafileIDs(dir string) ([]uint32, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*"+datafileExt))
	if err != nil {
		return nil, fmt.Errorf("storage: list data files: %w", err)
	}
	ids := make([]uint32, 0, len(matches))
	for _, path := range matches {
		name := strings.TrimSuffix(filepath.Base(path), datafileExt)
		id, err := strconv.ParseUint(name, 10, 32)
		if err != nil {
			// Not one of ours; leave it alone.
			continue
		}
		ids = append(ids, uint32(id))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// Get returns the value stored under key, or ErrNotFound.
func (b *Bitcask) Get(key []byte) ([]byte, error) {
	e, ok := b.keydir.get(string(key))
	if !ok {
		return nil, ErrNotFound
	}
	if e.valueSize == TombstoneValueSize {
		return nil, ErrNotFound
	}

	// Hold fmu across the read so Close can't close the file under us.
	b.fmu.RLock()
	defer b.fmu.RUnlock()
	df := b.files[e.fileID]

	if b.closed {
		return nil, ErrClosed
	}
	if df == nil {
		return nil, fmt.Errorf("storage: keydir points at missing file %d: %w", e.fileID, ErrCorrupt)
	}

	// The keydir holds the value offset. The record starts headerSize+len(key)
	// bytes earlier, so one ReadAt covers the whole record and its CRC.
	recordOff := e.valueOff - int64(headerSize) - int64(len(key))
	if recordOff < 0 {
		return nil, fmt.Errorf("storage: value offset %d is before the record start: %w", e.valueOff, ErrCorrupt)
	}

	buf := make([]byte, int64(headerSize)+int64(len(key))+int64(e.valueSize))
	if err := df.readAt(buf, recordOff); err != nil {
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	rec, err := decodeRecord(buf)
	if err != nil {
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	if !bytes.Equal(rec.Key, key) {
		return nil, fmt.Errorf("storage: record at %d holds key %q, want %q: %w", recordOff, rec.Key, key, ErrCorrupt)
	}
	if rec.Tombstone {
		return nil, ErrNotFound
	}
	return rec.Value, nil
}

// Put appends a record to the active file and updates the keydir. The first
// Put into an empty data dir creates the active file, and a record that would
// push the active file past MaxFileSize goes into a new one.
func (b *Bitcask) Put(key, value []byte) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()

	// Close also takes wmu, so neither closed nor active can change under us.
	b.fmu.RLock()
	closed, active := b.closed, b.active
	b.fmu.RUnlock()
	if closed {
		return ErrClosed
	}
	if active == nil {
		df, err := createDatafile(b.opts.Dir, b.nextID)
		if err != nil {
			return fmt.Errorf("storage: %w", err)
		}
		b.fmu.Lock()
		b.files[df.id] = df
		b.active = df
		b.nextID++
		b.fmu.Unlock()
		active = df
	}

	// Monotonic: a clock that jumps backwards must not make this write lose
	// to an older one on replay.
	ts := max(time.Now().UnixNano(), b.lastTS+1)
	buf, err := encodeRecord(ts, key, value, false)
	if err != nil {
		return err
	}
	// size > 0: a record larger than MaxFileSize still gets a file of its own.
	if active.size > 0 && active.size+int64(len(buf)) > b.opts.MaxFileSize {
		if active, err = b.rotate(active); err != nil {
			return fmt.Errorf("storage: put %q: %w", key, err)
		}
	}
	off, err := active.append(buf)
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	b.lastTS = ts
	if b.opts.Fsync == FsyncAlways {
		if err := active.sync(); err != nil {
			return fmt.Errorf("storage: put %q: %w", key, err)
		}
	}

	b.keydir.put(string(key), entry{
		fileID:    active.id,
		valueOff:  off + int64(headerSize) + int64(len(key)),
		valueSize: uint32(len(value)),
		timestamp: ts,
	})
	return nil
}

// rotate makes old immutable and starts a new active file. The caller holds
// wmu. old is synced first so an immutable file is always fully durable, then
// reopened read-only; readers still using the old handle finish before the swap.
func (b *Bitcask) rotate(old *datafile) (*datafile, error) {
	if err := old.sync(); err != nil {
		return nil, err
	}
	ro, err := openDatafile(b.opts.Dir, old.id, true)
	if err != nil {
		return nil, err
	}
	df, err := createDatafile(b.opts.Dir, b.nextID)
	if err != nil {
		ro.close()
		return nil, err
	}

	b.fmu.Lock()
	b.files[old.id] = ro
	b.files[df.id] = df
	b.active = df
	b.nextID++
	b.fmu.Unlock()

	if err := old.close(); err != nil {
		b.log.Warn("storage: close rotated data file", "err", err)
	}
	return df, nil
}

// Delete appends a tombstone and drops the key from the keydir.
//
// TODO(build order 5): implement tombstones.
func (b *Bitcask) Delete(key []byte) error {
	return ErrNotImplemented
}

// Scan walks every keydir key with the given prefix. The hash index is
// unordered, so this is O(n) over all live keys.
//
// TODO(build order 7): implement the scan.
func (b *Bitcask) Scan(prefix []byte, fn func(key, value []byte) bool) error {
	return ErrNotImplemented
}

// Close stops the interval syncer, fsyncs the active file and closes every
// open data file. It waits for an in-flight Put. It is safe to call twice.
func (b *Bitcask) Close() error {
	b.wmu.Lock()
	defer b.wmu.Unlock()

	b.fmu.Lock()
	if b.closed {
		b.fmu.Unlock()
		return nil
	}
	b.closed = true
	files, active := b.files, b.active
	b.files = nil
	b.active = nil
	b.fmu.Unlock()

	// fmu is released first: the syncer may be waiting on it.
	if b.stopSync != nil {
		close(b.stopSync)
		<-b.syncDone
	}

	var firstErr error
	if active != nil {
		firstErr = active.sync()
	}
	for _, df := range files {
		if err := df.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Closing the file releases the flock.
	if err := b.lock.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		return fmt.Errorf("storage: close: %w", firstErr)
	}
	return nil
}
