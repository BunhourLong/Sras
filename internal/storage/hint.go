package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	hintExt        = ".hint"
	hintHeaderSize = 13 // magic (4), version (1), data size (8)
	hintEntrySize  = 24 // timestamp (8), key size (4), value size (4), value offset (8)
)

type hintEntry struct {
	key string
	entry
}

func hintPath(dir string, id uint32) string {
	return filepath.Join(dir, fmt.Sprintf("%06d%s", id, hintExt))
}

// writeHint writes a synced, closed .hint.tmp for one merge output.
func writeHint(dir string, id uint32, dataSize int64, entries []hintEntry) error {
	buf := append([]byte("SRHT\x01"), make([]byte, 8)...)
	binary.LittleEndian.PutUint64(buf[5:13], uint64(dataSize))
	for _, e := range entries {
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e.timestamp))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(e.key)))
		buf = binary.LittleEndian.AppendUint32(buf, e.valueSize)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e.valueOff))
		buf = append(buf, e.key...)
	}
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
	f, err := os.OpenFile(hintPath(dir, id)+tmpExt, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create hint: %w", err)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return fmt.Errorf("write hint: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync hint: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close hint: %w", err)
	}
	return nil
}

// readHint validates the whole hint before returning any entries. Merge hints
// cover every record in their data file, in order, without gaps or tombstones.
// shortcut: the hint is bound to its .data by size and layout only, so a
// CRC-valid hint with forged keys or timestamps is trusted; IDs are never
// reused and old hints die before their data. Store a data-file checksum
// verified lazily if hints ever come from outside Merge.
func readHint(dir string, id uint32, dataSize int64) ([]hintEntry, error) {
	path := hintPath(dir, id)
	buf, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read hint %s: %w", path, err)
		}
		return nil, fmt.Errorf("read hint %s: %w: %w", path, err, ErrCorrupt)
	}
	bad := func(reason string) ([]hintEntry, error) {
		return nil, fmt.Errorf("hint %s: %s: %w", path, reason, ErrCorrupt)
	}
	if len(buf) < hintHeaderSize+crcSize {
		return bad("short header or trailer")
	}
	end := len(buf) - crcSize
	if string(buf[:4]) != "SRHT" || buf[4] != 1 {
		return bad("invalid magic or version")
	}
	if crc32.ChecksumIEEE(buf[:end]) != binary.LittleEndian.Uint32(buf[end:]) {
		return bad("CRC mismatch")
	}
	if dataSize < 0 || binary.LittleEndian.Uint64(buf[5:13]) != uint64(dataSize) {
		return bad("data size mismatch")
	}
	var entries []hintEntry
	var dataEnd int64
	for off := hintHeaderSize; off < end; {
		if end-off < hintEntrySize {
			return bad("short entry")
		}
		timestamp := int64(binary.LittleEndian.Uint64(buf[off : off+8]))
		keySize := binary.LittleEndian.Uint32(buf[off+8 : off+12])
		valueSize := binary.LittleEndian.Uint32(buf[off+12 : off+16])
		valueOff := int64(binary.LittleEndian.Uint64(buf[off+16 : off+24]))
		off += hintEntrySize
		if keySize == 0 || uint64(keySize) > uint64(end-off) {
			return bad("invalid key size")
		}
		if valueOff < headerSize+int64(keySize) || valueOff > dataSize ||
			int64(valueSize) > dataSize-valueOff || valueSize == TombstoneValueSize {
			return bad("invalid value bounds")
		}
		if valueOff-headerSize-int64(keySize) != dataEnd {
			return bad("noncontiguous records")
		}
		dataEnd = valueOff + int64(valueSize)
		entries = append(entries, hintEntry{
			key:   string(buf[off : off+int(keySize)]),
			entry: entry{fileID: id, valueOff: valueOff, valueSize: valueSize, timestamp: timestamp},
		})
		off += int(keySize)
	}
	if dataEnd != dataSize {
		return bad("incomplete data coverage")
	}
	return entries, nil
}

// removeOrphanHints removes numbered hints whose data files are absent.
func removeOrphanHints(dir string, ids []uint32) error {
	live := make(map[uint32]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*"+hintExt))
	if err != nil {
		return fmt.Errorf("storage: list hints: %w", err)
	}
	for _, path := range paths {
		id, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(path), hintExt), 10, 32)
		if err != nil || path != hintPath(dir, uint32(id)) || live[uint32(id)] {
			continue
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("storage: remove orphan hint: %w", err)
		}
	}
	return nil
}
