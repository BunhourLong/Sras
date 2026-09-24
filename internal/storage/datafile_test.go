package storage

import (
	"bytes"
	"testing"
)

func TestDatafileAppendReadAt(t *testing.T) {
	dir := t.TempDir()
	d, err := createDatafile(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if _, err := createDatafile(dir, 1); err == nil {
		t.Fatal("createDatafile on an existing file: want error")
	}

	keys := [][]byte{[]byte("a/1"), []byte("b/22")}
	vals := [][]byte{[]byte("one"), []byte("two-two")}
	var offs []int64
	for i := range keys {
		buf, err := encodeRecord(int64(i), keys[i], vals[i], false)
		if err != nil {
			t.Fatal(err)
		}
		off, err := d.append(buf)
		if err != nil {
			t.Fatal(err)
		}
		offs = append(offs, off)
	}
	if err := d.sync(); err != nil {
		t.Fatal(err)
	}

	for i := range keys {
		valueOff := offs[i] + headerSize + int64(len(keys[i]))
		got := make([]byte, len(vals[i]))
		if err := d.readAt(got, valueOff); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, vals[i]) {
			t.Fatalf("record %d: value %q, want %q", i, got, vals[i])
		}
	}
}
