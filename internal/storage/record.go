package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

// On-disk record layout, little endian:
//
//	| crc32 (4) | timestamp (8) | keySize (4) | valueSize (4) | key | value |
//
// The CRC covers every byte after the CRC field itself.
const (
	crcSize    = 4
	headerSize = 20

	// TombstoneValueSize marks a delete: the header carries this value size and
	// no value bytes follow it.
	TombstoneValueSize = math.MaxUint32
)

// header is the fixed-size prefix of every record.
type header struct {
	CRC       uint32
	Timestamp int64
	KeySize   uint32
	ValueSize uint32
}

// Record is one decoded entry from a data file.
type Record struct {
	Timestamp int64
	Key       []byte
	Value     []byte
	Tombstone bool
}

// decodeHeader reads the fixed-size header at the start of buf.
func decodeHeader(buf []byte) (header, error) {
	if len(buf) < headerSize {
		return header{}, fmt.Errorf("header is %d bytes, want %d: %w", len(buf), headerSize, ErrCorrupt)
	}
	return header{
		CRC:       binary.LittleEndian.Uint32(buf[0:4]),
		Timestamp: int64(binary.LittleEndian.Uint64(buf[4:12])),
		KeySize:   binary.LittleEndian.Uint32(buf[12:16]),
		ValueSize: binary.LittleEndian.Uint32(buf[16:20]),
	}, nil
}

// decodeRecord decodes one whole record from buf and verifies its CRC. The
// returned key and value alias buf.
func decodeRecord(buf []byte) (Record, error) {
	h, err := decodeHeader(buf)
	if err != nil {
		return Record{}, err
	}

	tombstone := h.ValueSize == TombstoneValueSize
	valueSize := 0
	if !tombstone {
		valueSize = int(h.ValueSize)
	}

	size := headerSize + int(h.KeySize) + valueSize
	if len(buf) < size {
		return Record{}, fmt.Errorf("record is %d bytes, want %d: %w", len(buf), size, ErrCorrupt)
	}
	if got := crc32.ChecksumIEEE(buf[crcSize:size]); got != h.CRC {
		return Record{}, fmt.Errorf("crc is %08x, want %08x: %w", got, h.CRC, ErrCorrupt)
	}

	rec := Record{Timestamp: h.Timestamp, Tombstone: tombstone}
	rec.Key = buf[headerSize : headerSize+int(h.KeySize)]
	if !tombstone {
		rec.Value = buf[headerSize+int(h.KeySize) : size]
	}
	return rec, nil
}

// encodeRecord builds one on-disk record. A tombstone carries
// TombstoneValueSize in the header and no value bytes; value is ignored.
func encodeRecord(timestamp int64, key, value []byte, tombstone bool) ([]byte, error) {
	if uint64(len(key)) > math.MaxUint32 {
		return nil, fmt.Errorf("storage: key is %d bytes, max %d", len(key), uint32(math.MaxUint32))
	}
	valueSize := uint32(TombstoneValueSize)
	if tombstone {
		value = nil
	} else {
		// MaxUint32 itself is reserved for tombstones.
		if uint64(len(value)) >= math.MaxUint32 {
			return nil, fmt.Errorf("storage: value is %d bytes, max %d", len(value), uint32(math.MaxUint32-1))
		}
		valueSize = uint32(len(value))
	}

	buf := make([]byte, headerSize+len(key)+len(value))
	binary.LittleEndian.PutUint64(buf[4:12], uint64(timestamp))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(len(key)))
	binary.LittleEndian.PutUint32(buf[16:20], valueSize)
	n := copy(buf[headerSize:], key)
	copy(buf[headerSize+n:], value)
	binary.LittleEndian.PutUint32(buf[0:crcSize], crc32.ChecksumIEEE(buf[crcSize:]))
	return buf, nil
}
