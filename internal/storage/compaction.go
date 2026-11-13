package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// tmpExt marks a merge output that is still being written. Open deletes them.
const tmpExt = ".tmp"

// moved is a live record Merge copied from an old file to an output file.
type moved struct {
	key      string
	old, new entry
}

// Merge compacts the immutable files: snapshot the immutable file list, write
// only the entries the keydir still points at into new merged files, swap the
// keydir entries over and delete the old files. The active file is never
// touched and reads and writes keep working throughout.
//
// Tombstones are always dropped: the snapshot holds every immutable file, and
// the files outside it only hold newer records, so no older copy of a deleted
// key survives the merge.
//
// Outputs keep their records' timestamps but take new IDs from nextID, above
// the active file, so replay relies on timestamps (see rebuildKeydir).
func (b *Bitcask) Merge() error {
	b.mergeMu.Lock()
	defer b.mergeMu.Unlock()

	b.fmu.RLock()
	closed := b.closed
	var olds []*datafile
	for _, df := range b.files {
		if df != b.active {
			olds = append(olds, df)
		}
	}
	b.fmu.RUnlock()
	if closed {
		return ErrClosed
	}
	if len(olds) == 0 {
		return nil
	}
	// Ascending, so a crash mid-delete leaves a suffix: a key's newest record
	// (its tombstone, if deleted) is in the highest file holding the key.
	sort.Slice(olds, func(i, j int) bool { return olds[i].id < olds[j].id })

	outs, moves, err := b.copyLive(olds)
	if err == nil {
		err = b.installMerge(olds, outs, moves)
	}
	if err != nil {
		// Outputs are never in b.files on error, and the keydir still points at
		// the old files, so drop renamed outputs too: a stray .data the engine
		// doesn't track would outlive a later merge that drops its tombstones.
		for _, o := range outs {
			hint := hintPath(b.opts.Dir, o.id)
			for _, p := range []string{o.path, datafilePath(b.opts.Dir, o.id), hint + tmpExt, hint} {
				_ = os.Remove(p) // missing ones are fine
			}
		}
		return fmt.Errorf("storage: merge: %w", err)
	}

	// Nothing reaches the old files any more: they're off the map and the
	// keydir no longer points at them.
	for _, df := range olds {
		if err := df.close(); err != nil {
			b.log.Warn("storage: merge: close old data file", "err", err)
		}
		if err := os.Remove(hintPath(b.opts.Dir, df.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("storage: merge: remove old hint: %w", err)
		}
		if err := os.Remove(df.path); err != nil {
			return fmt.Errorf("storage: merge: %w", err)
		}
	}
	return nil
}

// copyLive copies every record the keydir still points at from olds into new
// .tmp output files, each capped at MaxFileSize. The outputs come back synced
// and closed. It holds no lock while copying.
func (b *Bitcask) copyLive(olds []*datafile) (outs []*datafile, moves []moved, err error) {
	var out *datafile
	for _, df := range olds {
		end, err := df.records(func(off int64, rec Record) error {
			if rec.Tombstone {
				return nil
			}
			key := string(rec.Key)
			e, ok := b.keydir.get(key)
			if !ok || e.fileID != df.id || e.valueOff != off+int64(headerSize)+int64(len(key)) {
				return nil // overwritten or deleted
			}
			buf, err := encodeRecord(rec.Timestamp, rec.Key, rec.Value, false)
			if err != nil {
				return err
			}
			if out == nil || (out.size > 0 && out.size+int64(len(buf)) > b.opts.MaxFileSize) {
				if out, err = b.createMergeFile(); err != nil {
					return err
				}
				outs = append(outs, out)
			}
			start, err := out.append(buf)
			if err != nil {
				return err
			}
			ne := e
			ne.fileID, ne.valueOff = out.id, start+int64(headerSize)+int64(len(key))
			moves = append(moves, moved{key: key, old: e, new: ne})
			return nil
		})
		if err == nil && end != df.size {
			err = fmt.Errorf("%s: invalid record at offset %d: %w", df.path, end, ErrCorrupt)
		}
		if err != nil {
			closeAll(outs)
			return outs, nil, err
		}
	}
	for _, o := range outs {
		if err := o.sync(); err != nil {
			closeAll(outs)
			return outs, nil, err
		}
	}
	closeAll(outs)
	return outs, moves, nil
}

func closeAll(dfs []*datafile) {
	for _, df := range dfs {
		_ = df.close()
	}
}

// createMergeFile reserves the next file ID and creates NNNNNN.data.tmp for it.
func (b *Bitcask) createMergeFile() (*datafile, error) {
	b.wmu.Lock()
	b.fmu.Lock()
	id := b.nextID
	b.nextID++
	b.fmu.Unlock()
	b.wmu.Unlock()

	path := datafilePath(b.opts.Dir, id) + tmpExt
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create merge file: %w", err)
	}
	return &datafile{id: id, path: path, f: f}, nil
}

// installMerge makes the outputs live and takes olds out of the engine, under
// wmu. The on-disk steps are ordered so a crash at any point replays correctly.
func (b *Bitcask) installMerge(olds, outs []*datafile, moves []moved) error {
	// copyLive has finished; build each hint in data-file order before taking
	// the write lock, so ordinary writes can continue during hint I/O.
	hints := make(map[uint32][]hintEntry, len(outs))
	for _, m := range moves {
		hints[m.new.fileID] = append(hints[m.new.fileID], hintEntry{key: m.key, entry: m.new})
	}
	for _, o := range outs {
		if err := writeHint(b.opts.Dir, o.id, o.size, hints[o.id]); err != nil {
			return err
		}
	}

	b.wmu.Lock()
	defer b.wmu.Unlock()

	var ros []*datafile
	if len(outs) > 0 {
		// Outputs have IDs above the active file. Rotate so every later write
		// lands above them too.
		if _, err := b.rotate(b.active); err != nil {
			return err
		}
		// A key copied and then deleted mid-merge has its tombstone below the
		// output, so replay would resurrect the copy. Tombstone it again above.
		for _, m := range moves {
			if _, ok := b.keydir.get(m.key); !ok {
				if _, err := b.appendRecord([]byte(m.key), nil, true); err != nil {
					return err
				}
			}
		}
		if err := b.active.sync(); err != nil {
			return err
		}
		for _, o := range outs {
			if err := os.Rename(o.path, datafilePath(b.opts.Dir, o.id)); err != nil {
				closeAll(ros)
				return fmt.Errorf("rename merge file: %w", err)
			}
			hint := hintPath(b.opts.Dir, o.id)
			if err := os.Rename(hint+tmpExt, hint); err != nil {
				closeAll(ros)
				return fmt.Errorf("rename merge hint: %w", err)
			}
			ro, err := openDatafile(b.opts.Dir, o.id, true)
			if err != nil {
				closeAll(ros)
				return err
			}
			ros = append(ros, ro)
		}
		if err := syncDir(b.opts.Dir); err != nil {
			closeAll(ros)
			return err
		}
	}

	b.fmu.Lock()
	for _, ro := range ros {
		b.files[ro.id] = ro
	}
	for _, m := range moves {
		// A Put or Delete during the merge wins.
		if e, ok := b.keydir.get(m.key); ok && e == m.old {
			b.keydir.put(m.key, m.new)
		}
	}
	for _, df := range olds {
		delete(b.files, df.id)
	}
	b.fmu.Unlock()
	return nil
}

// syncDir fsyncs dir so renames and new file names in it are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync dir %s: %w", dir, err)
	}
	return nil
}

// removeMergeTemps deletes .tmp outputs left by a merge that didn't finish.
func removeMergeTemps(dir string) error {
	for _, ext := range []string{datafileExt, hintExt} {
		tmps, err := filepath.Glob(filepath.Join(dir, "*"+ext+tmpExt))
		if err != nil {
			return fmt.Errorf("storage: list merge temp files: %w", err)
		}
		for _, p := range tmps {
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("storage: remove merge temp file: %w", err)
			}
		}
	}
	return nil
}
