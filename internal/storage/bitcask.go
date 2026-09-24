package storage

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var _ Engine = (*Bitcask)(nil)

// Bitcask is an append-only log store with an in-memory hash index. There is a
// single writer (wmu); readers run concurrently against the immutable files.
type Bitcask struct {
	opts Options
	log  *slog.Logger

	wmu sync.Mutex // serialises writers

	fmu    sync.RWMutex // guards the fields below
	files  map[uint32]*datafile
	active *datafile
	nextID uint32
	closed bool

	keydir *keydir

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

	ids, err := listDatafileIDs(opts.Dir)
	if err != nil {
		return nil, err
	}

	b := &Bitcask{
		opts:   opts,
		log:    opts.Logger,
		files:  make(map[uint32]*datafile, len(ids)),
		nextID: 1,
		keydir: newKeydir(),
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

	if err := b.rebuildKeydir(); err != nil {
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
			b.fmu.RLock()
			active := b.active
			b.fmu.RUnlock()
			if active == nil {
				continue
			}
			if err := active.sync(); err != nil {
				b.log.Error("storage: interval sync", "err", err)
			}
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

	b.fmu.RLock()
	closed := b.closed
	df := b.files[e.fileID]
	b.fmu.RUnlock()

	if closed {
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
// Put into an empty data dir creates the active file.
//
// TODO(build order 4): rotate when the active file exceeds MaxFileSize.
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

	ts := time.Now().UnixNano()
	buf, err := encodeRecord(ts, key, value, false)
	if err != nil {
		return err
	}
	off, err := active.append(buf)
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
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
	if firstErr != nil {
		return fmt.Errorf("storage: close: %w", firstErr)
	}
	return nil
}
