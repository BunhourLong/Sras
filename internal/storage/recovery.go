package storage

import (
	"errors"
	"fmt"
	"os"
)

// rebuildKeydir uses immutable hints or scans data files in ID order (ids
// ascending): the latest timestamp wins (ties go to the later record)
// and a tombstone removes the key. A corrupt or truncated tail on the active
// file is truncated at the last valid record and logged; corruption in an
// immutable file without a valid hint is ErrCorrupt. With a valid hint the
// immutable .data is not scanned at Open, so value corruption surfaces at Get
// (CRC) instead of ErrCorrupt at Open. A bad CRC never panics.
func (b *Bitcask) rebuildKeydir(ids []uint32) error {
	for _, id := range ids {
		df := b.files[id]
		if df != b.active {
			entries, err := readHint(b.opts.Dir, id, df.size)
			if err == nil {
				for _, e := range entries {
					b.lastTS = max(b.lastTS, e.timestamp)
					if cur, ok := b.keydir.get(e.key); ok && e.timestamp < cur.timestamp {
						continue
					}
					b.keydir.put(e.key, e.entry)
				}
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				b.log.Warn("storage: ignoring invalid hint; scanning data file", "file", hintPath(b.opts.Dir, id), "err", err)
			}
		}
		validEnd, err := df.records(func(off int64, rec Record) error {
			b.lastTS = max(b.lastTS, rec.Timestamp)
			key := string(rec.Key)
			if cur, ok := b.keydir.get(key); ok && rec.Timestamp < cur.timestamp {
				return nil // an older record, e.g. from a merged file
			}
			if rec.Tombstone {
				b.keydir.delete(key)
				return nil
			}
			b.keydir.put(key, entry{
				fileID:    id,
				valueOff:  off + int64(headerSize) + int64(len(rec.Key)),
				valueSize: uint32(len(rec.Value)),
				timestamp: rec.Timestamp,
			})
			return nil
		})
		if err != nil {
			return err
		}
		if validEnd == df.size {
			continue
		}
		if df != b.active {
			// Immutable files were fully synced before rotation, so this is
			// real damage, not a torn write.
			return fmt.Errorf("%s: invalid record at offset %d: %w", df.path, validEnd, ErrCorrupt)
		}
		dropped := df.size - validEnd
		if err := df.truncate(validEnd); err != nil {
			return err
		}
		b.log.Warn("storage: truncated corrupt tail of active data file",
			"file", df.path, "offset", validEnd, "bytesDropped", dropped)
	}
	return nil
}
