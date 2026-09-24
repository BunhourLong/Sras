package storage

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeDecodeRecord(t *testing.T) {
	tests := []struct {
		name      string
		key, val  []byte
		tombstone bool
	}{
		{"value", []byte("users/1"), []byte(`{"a":1}`), false},
		{"empty value", []byte("k"), []byte{}, false},
		{"tombstone", []byte("users/1"), []byte("ignored"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf, err := encodeRecord(42, tt.key, tt.val, tt.tombstone)
			if err != nil {
				t.Fatal(err)
			}
			rec, err := decodeRecord(buf)
			if err != nil {
				t.Fatal(err)
			}
			if rec.Timestamp != 42 || !bytes.Equal(rec.Key, tt.key) || rec.Tombstone != tt.tombstone {
				t.Fatalf("got %+v", rec)
			}
			if !tt.tombstone && !bytes.Equal(rec.Value, tt.val) {
				t.Fatalf("value %q, want %q", rec.Value, tt.val)
			}

			buf[len(buf)-1] ^= 0xff
			if _, err := decodeRecord(buf); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("flipped byte: err %v, want ErrCorrupt", err)
			}
		})
	}
}
