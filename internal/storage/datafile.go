package storage

import (
	"bufio"
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

// openDatafile opens an existing data file, read-only unless it is the
// active file.
func openDatafile(dir string, id uint32, readOnly bool) (*datafile, error) {
	path := datafilePath(dir, id)
	flag := os.O_RDONLY
	if !readOnly {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, fmt.Errorf("open data file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat data file %s: %w", path, err)
	}
	return &datafile{id: id, path: path, f: f, readOnly: readOnly, size: info.Size()}, nil
}

// createDatafile creates a new, empty active data file. It fails if the file
// already exists.
func createDatafile(dir string, id uint32) (*datafile, error) {
	path := datafilePath(dir, id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create data file: %w", err)
	}
	return &datafile{id: id, path: path, f: f, readOnly: false}, nil
}

// append writes buf at the end of the file and returns the offset where it
// starts. A record's value offset is that plus headerSize + len(key).
func (d *datafile) append(buf []byte) (int64, error) {
	off := d.size
	if _, err := d.f.WriteAt(buf, off); err != nil {
		return 0, fmt.Errorf("write %s at %d: %w", d.path, off, err)
	}
	d.size += int64(len(buf))
	return off, nil
}

func (d *datafile) sync() error {
	if err := d.f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", d.path, err)
	}
	return nil
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

// records calls fn for each valid record from the start of the file, in
// order. It stops at the first record that is truncated, claims more bytes than
// the file holds, or fails its CRC, and returns validEnd: the offset where valid
// data ends. validEnd < d.size means the rest of the file is bad. err is only an
// I/O error or an error returned by fn.
func (d *datafile) records(fn func(off int64, rec Record) error) (validEnd int64, err error) {
	r := bufio.NewReaderSize(io.NewSectionReader(d.f, 0, d.size), 64<<10)
	hdr := make([]byte, headerSize)
	var off int64
	for off < d.size {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return off, nil // torn header
			}
			return off, fmt.Errorf("read %s at %d: %w", d.path, off, err)
		}
		h, _ := decodeHeader(hdr) // hdr is always headerSize bytes
		bodySize := int64(h.KeySize)
		if h.ValueSize != TombstoneValueSize {
			bodySize += int64(h.ValueSize)
		}
		// Check sizes before allocating, so garbage can't ask for gigabytes.
		if bodySize > d.size-off-headerSize {
			return off, nil
		}
		buf := make([]byte, headerSize+bodySize)
		copy(buf, hdr)
		if _, err := io.ReadFull(r, buf[headerSize:]); err != nil {
			return off, fmt.Errorf("read %s at %d: %w", d.path, off, err)
		}
		rec, err := decodeRecord(buf)
		if err != nil {
			return off, nil // bad CRC
		}
		if err := fn(off, rec); err != nil {
			return off, err
		}
		off += int64(len(buf))
	}
	return off, nil
}

// truncate cuts the file to size bytes and syncs it.
func (d *datafile) truncate(size int64) error {
	if err := d.f.Truncate(size); err != nil {
		return fmt.Errorf("truncate %s to %d: %w", d.path, size, err)
	}
	d.size = size
	return d.sync()
}

func (d *datafile) close() error {
	if err := d.f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", d.path, err)
	}
	return nil
}
