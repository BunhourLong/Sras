// Package service owns documents and collections: ID generation, _rev handling,
// metadata and conflict checks. It calls the query and storage layers.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"sras/internal/storage"
)

// Reserved document fields, managed by this layer.
const (
	FieldID        = "_id"
	FieldRev       = "_rev"
	FieldUpdatedAt = "_updatedAt"
)

// maxIDLen bounds a document ID so a key stays a sane size.
const maxIDLen = 256

// Sentinel errors returned by the service layer.
var (
	ErrNotFound          = errors.New("service: document not found")
	ErrConflict          = errors.New("service: revision conflict")
	ErrInvalidCollection = errors.New("service: invalid collection name")
	ErrInvalidID         = errors.New("service: invalid document id")
	ErrInvalidDocument   = errors.New("service: invalid document")
)

// Document is a stored JSON document, including the reserved fields.
type Document map[string]any

// Documents reads and writes documents through the storage engine.
type Documents struct {
	engine storage.Engine
	log    *slog.Logger

	// wmu makes Put's read-check-write atomic.
	// ponytail: one lock for every document; per-key locks if writes contend.
	wmu sync.Mutex
}

// NewDocuments wires the document service to an engine.
func NewDocuments(engine storage.Engine, log *slog.Logger) *Documents {
	if log == nil {
		log = slog.Default()
	}
	return &Documents{engine: engine, log: log}
}

// Get returns one document, or ErrNotFound.
func (d *Documents) Get(ctx context.Context, collection, id string) (Document, error) {
	if err := validateCollection(collection); err != nil {
		return nil, err
	}
	if err := validateID(id); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	raw, err := d.engine.Get([]byte(documentKey(collection, id)))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("get %s/%s: %w", collection, id, ErrNotFound)
		}
		return nil, fmt.Errorf("get %s/%s: %w", collection, id, err)
	}

	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode %s/%s: %w", collection, id, err)
	}
	return doc, nil
}

// Put creates or replaces a document and returns it with the reserved fields
// set. The body's _rev must match the stored one (a missing _rev counts as 0,
// which is what a document that does not exist yet has); otherwise Put returns
// ErrConflict. created reports whether the document was new.
func (d *Documents) Put(ctx context.Context, collection, id string, doc Document) (saved Document, created bool, err error) {
	if err := validateCollection(collection); err != nil {
		return nil, false, err
	}
	if err := validateID(id); err != nil {
		return nil, false, err
	}
	if doc == nil {
		return nil, false, fmt.Errorf("body must be a JSON object: %w", ErrInvalidDocument)
	}
	wantRev, err := revOf(doc)
	if err != nil {
		return nil, false, err
	}

	d.wmu.Lock()
	defer d.wmu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	var curRev int64
	cur, err := d.Get(ctx, collection, id)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return nil, false, err
	default:
		if curRev, err = revOf(cur); err != nil {
			return nil, false, fmt.Errorf("stored %s/%s: %w", collection, id, err)
		}
	}
	if wantRev != curRev {
		return nil, false, fmt.Errorf("put %s/%s: _rev %d, current %d: %w", collection, id, wantRev, curRev, ErrConflict)
	}

	doc[FieldID] = id
	doc[FieldRev] = curRev + 1
	doc[FieldUpdatedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, false, fmt.Errorf("encode %s/%s: %w", collection, id, err)
	}
	if err := d.engine.Put([]byte(documentKey(collection, id)), raw); err != nil {
		return nil, false, fmt.Errorf("put %s/%s: %w", collection, id, err)
	}
	return doc, curRev == 0, nil
}

// revOf reads _rev as a non-negative integer; a missing _rev is 0. JSON
// numbers decode as float64, a freshly set one is int64.
func revOf(doc Document) (int64, error) {
	switch v := doc[FieldRev].(type) {
	case nil:
		return 0, nil
	case int64:
		return v, nil
	case float64:
		if v >= 0 && v == float64(int64(v)) {
			return int64(v), nil
		}
	}
	return 0, fmt.Errorf("_rev must be a non-negative integer: %w", ErrInvalidDocument)
}

// documentKey builds the storage key: <collection>/<id>.
func documentKey(collection, id string) string {
	return collection + "/" + id
}

// validateID rejects empty, oversized, or separator-bearing IDs.
func validateID(id string) error {
	if id == "" || len(id) > maxIDLen {
		return fmt.Errorf("%q: %w", id, ErrInvalidID)
	}
	if strings.ContainsAny(id, "/\x00") {
		return fmt.Errorf("%q: %w", id, ErrInvalidID)
	}
	return nil
}
