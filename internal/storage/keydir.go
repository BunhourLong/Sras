package storage

import (
	"strings"
	"sync"
)

// entry records where a key's most recent value lives on disk.
type entry struct {
	fileID    uint32
	valueOff  int64
	valueSize uint32
	timestamp int64
}

// keydir is the in-memory hash index: every live key maps to one entry. It is
// unordered, so a prefix scan has to walk the whole map.
type keydir struct {
	mu      sync.RWMutex
	entries map[string]entry
}

func newKeydir() *keydir {
	return &keydir{entries: make(map[string]entry)}
}

func (k *keydir) get(key string) (entry, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	e, ok := k.entries[key]
	return e, ok
}

func (k *keydir) put(key string, e entry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.entries[key] = e
}

func (k *keydir) delete(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.entries, key)
}

// keys returns a snapshot of every key with the given prefix, unordered.
// ponytail: O(n) over all keys; an ordered index (roadmap 4.2) makes it O(log n + k).
func (k *keydir) keys(prefix string) []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	var out []string
	for key := range k.entries {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	return out
}

func (k *keydir) len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.entries)
}
