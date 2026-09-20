package storage

// rebuildKeydir replays every data file in ID order to rebuild the in-memory
// index: the latest timestamp wins and a tombstone removes the key. A corrupt
// or truncated tail on the active file is truncated at the last valid record
// and logged; a bad CRC never panics.
//
// TODO(build order 4/5): implement the replay. Until then Open starts with an
// empty keydir, so Get reports ErrNotFound for every key.
func (b *Bitcask) rebuildKeydir() error {
	return nil
}
