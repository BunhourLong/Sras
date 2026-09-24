package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	datafileExt    = ".data"
	datafileFormat = "%06d" + datafileExt
)

// datafile is one file in the data dir. Exactly one datafile is active and
// append-only; every older one is immutable and opened read-only.
type datafile struct {
	id       uint32
	path     string
	f        *os.File
	readOnly bool
	size     int64 // bytes on disk; the append offset for the active file
}

// datafilePath returns the path of the data file with this ID.
func datafilePath(dir string, id uint32) string {
	return filepath.Join(dir, fmt.Sprintf(datafileFormat, id))
}

// openDatafile opens an existing data file read-only.
func openDatafile(dir string, id uint32) (*datafile, error) {
	path := datafilePath(dir, id)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open data file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat data file %s: %w", path, err)
	}
	return &datafile{id: id, path: path, f: f, readOnly: true, size: info.Size()}, nil
}

// readAt fills buf from off. A short read means the record is truncated, which
// is reported as ErrCorrupt.
func (d *datafile) readAt(buf []byte, off int64) error {
	if _, err := d.f.ReadAt(buf, off); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("short read of %d bytes at %d in %s: %w", len(buf), off, d.path, ErrCorrupt)
		}
		return fmt.Errorf("read %s at %d: %w", d.path, off, err)
	}
	return nil
}

func (d *datafile) close() error {
	if err := d.f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", d.path, err)
	}
	return nil
}
