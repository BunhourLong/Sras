package storage

// Merge compacts the immutable files: snapshot the immutable file list, write
// only the entries the keydir still points at into new merged files, swap the
// keydir entries over and delete the old files. The active file is never
// touched and reads and writes keep working throughout.
//
// TODO(build order 6): implement compaction.
func (b *Bitcask) Merge() error {
	return ErrNotImplemented
}
