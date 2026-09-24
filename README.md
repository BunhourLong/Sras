# Sras

An HTTP document database (NoSQL) written in Go, built on a **Bitcask-style** storage engine.

Clients store JSON documents in named collections through a small REST API. Underneath, every write is appended to a log file on disk, and an in-memory hash index remembers where the latest version of each key lives, so a read is one map lookup plus one disk read.

This is a learning project: **correctness and clarity come before performance**. It uses only the Go standard library.

> **Status:** work in progress. Documents can be created, replaced and read over HTTP, and survive a restart (the keydir is rebuilt on startup). Data files rotate at `MaxFileSize`. The engine supports delete (tombstones); the `DELETE` endpoint, scan, query and compaction are still pending. See [Roadmap](#roadmap).

---

## Contents
- [Quick start](#quick-start)
- [HTTP API](#http-api)
- [Document model](#document-model)
- [Architecture](#architecture)
- [Storage engine (Bitcask)](#storage-engine-bitcask)
- [Configuration](#configuration)
- [Project layout](#project-layout)
- [Testing](#testing)
- [Roadmap](#roadmap)
- [Known limitations](#known-limitations)
- [Contributing workflow](#contributing-workflow)

---

## Quick start

Requirements: **Go 1.22+** (the server uses the method/pattern router added in 1.22). No CGO, no external database.

```bash
git clone https://github.com/BunhourLong/Sras.git
cd Sras
go run ./cmd/server -data ./data -addr :8080
```

Then, in another terminal:

```bash
# health check
curl -s localhost:8080/healthz
# {"status":"ok"}

# create a document (no _rev needed when creating)
curl -s -X PUT localhost:8080/db/users/42 -d '{"name":"Ada","role":"admin"}'
# 201 {"_id":"42","_rev":1,"_updatedAt":"2026-09-24T10:00:00.123456789Z","name":"Ada","role":"admin"}

# read it back
curl -s localhost:8080/db/users/42

# replace it: send the current _rev
curl -s -X PUT localhost:8080/db/users/42 -d '{"name":"Ada Lovelace","_rev":1}'
# 200 {..., "_rev":2, ...}

# a stale _rev is rejected
curl -s -X PUT localhost:8080/db/users/42 -d '{"name":"old","_rev":1}'
# 409 {"error":{"code":"conflict","message":"revision conflict"}}
```

Stop the server with `Ctrl+C`. It shuts down gracefully: in-flight requests get up to 10 seconds, then the storage engine fsyncs the active file and closes.

---

## HTTP API

| Method | Path | Description | Status |
|---|---|---|---|
| `GET` | `/healthz` | Liveness check, returns `{"status":"ok"}` | ✅ implemented |
| `GET` | `/db/{collection}/{id}` | Read a document | ✅ implemented |
| `PUT` | `/db/{collection}/{id}` | Create (201) or replace (200) a document | ✅ implemented |
| `DELETE` | `/db/{collection}/{id}` | Delete a document (204) | ⏳ planned |
| `POST` | `/db/{collection}/_find` | Query with a filter | ⏳ planned |
| `POST` | `/admin/merge` | Trigger compaction | ⏳ planned |

### `PUT /db/{collection}/{id}`
- Body must be a JSON **object**, at most **1 MB**. Arrays, `null`, and invalid JSON return `400`.
- **Create:** omit `_rev` (or send `0`). Returns `201 Created` with the saved document.
- **Replace:** send the document's current `_rev`. Returns `200 OK` with `_rev` incremented.
- **Conflict:** a `_rev` that doesn't match the stored one returns `409 Conflict`. Sending no `_rev` for an existing document is also a conflict, so you can't overwrite by accident.
- `_rev` must be a non-negative integer, otherwise `400`.

### `GET /db/{collection}/{id}`
Returns `200` with the document, or `404` if it doesn't exist.

### `POST /db/{collection}/_find` (planned)
```json
{"filter": {"age": {"$gt": 30}, "role": {"$in": ["admin", "owner"]}}, "limit": 20, "skip": 0}
```
Supported operators will be `$eq`, `$gt`, `$lt`, `$in`; conditions on different fields are ANDed.

### Errors
Every error uses the same envelope:
```json
{"error": {"code": "not_found", "message": "document not found"}}
```

| Status | `code` | When |
|---|---|---|
| 400 | `bad_request` | Invalid collection name or ID, body not a JSON object, bad `_rev` |
| 404 | `not_found` | Document does not exist |
| 409 | `conflict` | `_rev` does not match the stored revision |
| 500 | `internal` | Anything else. The details are logged server-side, never returned to the client. |

---

## Document model

- **Collection names:** `[a-z0-9_-]{1,64}` (lowercase only; `/` and NUL are rejected).
- **Document IDs:** non-empty, at most 256 bytes, no `/` or NUL.
- **Storage key:** `<collection>/<id>`, for example `users/42`.
- **Reserved fields**, managed by the server (values you send for them are overwritten, except `_rev`, which is checked):

| Field | Type | Meaning |
|---|---|---|
| `_id` | string | Always set from the URL |
| `_rev` | integer | Starts at 1, incremented on every write. Used for optimistic concurrency. |
| `_updatedAt` | string | RFC 3339 timestamp (UTC, nanosecond precision) of the last write |

**Optimistic concurrency:** read a document, change it, and send it back with the `_rev` you read. If someone else wrote in the meantime, the `_rev` no longer matches and you get `409`. Re-read and retry.

---

## Architecture

Strict layering: each layer only calls the one directly below it.

```
HTTP API  ->  Service  ->  Query  ->  Storage (Bitcask)  ->  Disk
```

| Layer | Package | Responsibility |
|---|---|---|
| HTTP | `internal/api` | Routing, JSON decode/encode, status codes, middleware. No business logic. |
| Service | `internal/service` | Collections, documents, ID validation, `_rev`, metadata, conflict checks |
| Query | `internal/query` | Parse filters (`$eq`, `$gt`, `$lt`, `$in`), plan, evaluate against documents |
| Storage | `internal/storage` | Bitcask engine behind the `Engine` interface. Knows only `[]byte` keys and values. |

Rules:
- The storage layer never imports JSON, HTTP, or service types.
- The API layer never touches storage directly. It depends on a small `DocumentService` interface (`internal/api/router.go`).
- There is no global state. All dependencies are wired in `cmd/server/main.go`.

### Request flow: `PUT /db/users/42`
1. **api:** decode the body (max 1 MB) into a `service.Document`, call `docs.Put`.
2. **service:** validate collection and ID, read `_rev` from the body, take the write lock, load the current document, compare revisions, set `_id` / `_rev` / `_updatedAt`, marshal to JSON.
3. **storage:** encode a record, append it to the active data file, fsync if the policy says so, and point the keydir at the new value.
4. **api:** respond `201` (created) or `200` (replaced) with the saved document.

---

## Storage engine (Bitcask)

The design follows the [Bitcask paper](https://riak.com/assets/bitcask-intro.pdf) (Basho, 2010).

### Engine interface
```go
type Engine interface {
    Get(key []byte) ([]byte, error)          // ErrNotFound if missing
    Put(key, value []byte) error
    Delete(key []byte) error
    Scan(prefix []byte, fn func(key, value []byte) bool) error
    Merge() error                            // compaction
    Close() error
}
```

### On-disk record format (little endian)
```
+-----------+---------------+--------------+----------------+-----+-------+
| crc32 (4) | timestamp (8) | keySize (4)  | valueSize (4)  | key | value |
+-----------+---------------+--------------+----------------+-----+-------+
 \_________________________ 20-byte header _______________/
```
- The CRC32 (IEEE) covers every byte after the CRC field, so a bit flip anywhere in the header, key or value is detected.
- **Tombstone (delete):** `valueSize = 0xFFFFFFFF` and no value bytes follow. Because that value is reserved, a real value can be at most `2^32 - 2` bytes.
- The timestamp is `time.Now().UnixNano()` at write time.

### Data files
- Named `000001.data`, `000002.data`, ... in the data directory.
- Exactly one **active** file receives appends. All older files are **immutable** and opened read-only.
- The first `Put` into an empty directory creates `000001.data`. On `Open`, the newest file is reopened read-write and appends continue after its existing contents.
- Rotation happens in `Put` when the next record would push the active file past `MaxFileSize` (default 64 MB): the active file is fsynced, reopened read-only, and a new active file is created. A record larger than `MaxFileSize` gets a file of its own.

### Keydir (in-memory index)
```go
type entry struct {
    fileID    uint32  // which data file
    valueOff  int64   // byte offset of the value inside that file
    valueSize uint32
    timestamp int64
}
map[string]entry  // guarded by sync.RWMutex
```
Every live key has exactly one entry pointing at its latest value. The whole key set lives in RAM, and values stay on disk.

### Operations
| Operation | How it works | Cost |
|---|---|---|
| **Put** | Encode record, append to the active file, update the keydir. Keydir stores `recordStart + 20 + len(key)` as the value offset. | One sequential write |
| **Get** | Keydir lookup, then **one `ReadAt`** that covers the whole record (header + key + value). CRC and key are verified before returning. | One random read |
| **Delete** | Append a tombstone, remove the key from the keydir. A missing key returns `ErrNotFound` and writes nothing. Shares `Put`'s write path (`appendRecord`), so rotation and fsync apply. | One sequential write |
| **Scan(prefix)** *(planned)* | Walk every keydir key and match the prefix | **O(n)** over all keys, because a hash index is unordered |
| **Merge** *(planned)* | Rewrite immutable files, keeping only live records | Background I/O |

### Concurrency
- **Single writer:** `Put` holds a write mutex (`wmu`). Many readers run concurrently without it; `Get` holds `fmu` (read lock) across its `ReadAt`, so `Close` waits for in-flight reads.
- **One process per data dir:** `Open` takes an exclusive `flock` on `<dir>/LOCK` and fails with `ErrLocked` if it is held. The OS drops the lock if the process dies (Unix only).
- **Monotonic timestamps:** `Put` uses `max(now, lastTimestamp+1)`, so a clock jumping backwards can't make a newer write lose on replay.
- **Lock order:** `wmu` before `fmu` (the file-table lock). `Close` takes `wmu`, so it waits for an in-flight `Put` to finish.
- The service layer adds its own mutex around read-check-write in `Put`, so two concurrent `PUT`s to the same document cannot both succeed with the same `_rev`. It is one lock for all documents; per-key locks are a later optimization if writes contend.

### Durability (fsync policy)
| Policy | Behaviour | Risk on power loss |
|---|---|---|
| `always` | fsync inside every `Put` before it returns | None for acknowledged writes; slowest |
| `interval` *(default)* | A background goroutine fsyncs the active file every `SyncInterval` (1s) | Up to ~1s of writes |
| `never` | Leave it to the OS | Whatever the OS hadn't flushed |

`Close` always fsyncs the active file, whatever the policy.

### Recovery on `Open`
1. List data files in ID order.
2. Replay every record sequentially (64 KB buffered reader) to rebuild the keydir: latest timestamp wins (ties go to the later record), a tombstone removes the key.
3. If the active file has an invalid record (torn write, garbage, bad CRC), truncate it there and log a warning with the number of bytes dropped. An invalid record in an **immutable** file makes `Open` fail with `ErrCorrupt`.
4. Never panic on a bad CRC; sizes are checked against the file before allocating.

### Compaction (`Merge`) *(planned, step 6)*
1. Snapshot the list of immutable files.
2. Copy only live entries (those the keydir still points at) into new merged files.
3. Atomically swap the keydir entries, then delete the old files.
4. The active file is never touched; reads and writes keep working during a merge.

---

## Configuration

| Setting | Flag | Default | Notes |
|---|---|---|---|
| Listen address | `-addr` | `:8080` | |
| Data directory | `-data` | `./data` | Created if missing |
| Max data file size | — | 64 MB | Set in `internal/config`; no flag yet |
| Fsync policy | — | `interval` | `always` / `interval` / `never`; no flag yet |
| Sync interval | — | `1s` | Used when policy is `interval` |

Logs are structured (`log/slog`, text format) on stdout.

---

## Project layout

```
cmd/server/main.go          # flags, wiring, graceful shutdown
internal/api/
    router.go               # Server, DocumentService interface, routes
    handlers.go             # GET/PUT document, healthz
    errors.go               # error envelope, service error -> status mapping
    middleware.go           # (empty, reserved)
    handlers_test.go        # end-to-end CRUD via httptest + real engine
internal/service/
    documents.go            # Get, Put, _rev handling, ID validation
    collections.go          # collection name validation
internal/query/
    parser.go               # filter / FindRequest types
    eval.go                 # (empty, planned)
internal/storage/
    engine.go               # Engine interface, Options, sentinel errors
    bitcask.go              # Open / Get / Put / Delete / Scan / Close, interval syncer
    datafile.go             # one data file: open/create, readAt, append, sync
    record.go               # record encode/decode + CRC
    keydir.go               # in-memory index
    compaction.go           # Merge (planned)
    recovery.go             # keydir rebuild on Open
    *_test.go               # record, datafile, bitcask tests
internal/config/config.go   # defaults
data/                       # runtime data (gitignored)
```

---

## Testing

```bash
gofmt -l .           # should print nothing
go vet ./...
go test -race ./...
```

Current coverage:
- **`record_test.go`:** encode/decode round-trips (normal value, empty value, tombstone), plus a flipped byte in each case must return `ErrCorrupt`.
- **`datafile_test.go`:** creating an existing file fails; append two records, sync, and read each value back at `recordStart + 20 + len(key)`.
- **`bitcask_test.go`:** Put/Get/overwrite under all three fsync policies, `ErrClosed` after `Close`, reopening appends to the existing file, and a concurrent Put/Get test (8 goroutines × 100 keys) run with `-race`.
- **`handlers_test.go`:** full HTTP flow through `httptest` against a real engine in `t.TempDir()`: healthz, 404, create 201, replace 200, stale `_rev` 409, bad `_rev` / non-object / `null` body / bad collection 400.

Conventions: data directories always come from `t.TempDir()`, and every bug fix gets a regression test.

---

## Roadmap

| # | Step | Status |
|---|---|---|
| 1 | Service layer + HTTP CRUD | 🟡 `GET`, `PUT`, `/healthz` done; `DELETE` pending |
| 2 | `record.go` + `datafile.go` (encode/decode, CRC, append) | ✅ done |
| 3 | Bitcask `Put` / `Get` / `Delete` on a single file | ✅ done |
| 4 | File rotation + keydir rebuild on startup | ✅ done |
| 5 | Tombstones + truncated-tail recovery | ✅ done |
| 6 | Compaction (`Merge`) + `/admin/merge` | ⏳ |
| 7 | Query engine + `_find` endpoint | ⏳ types only |
| 8 | Optional: hint files, secondary indexes, TTL, auth | ⏳ |

---

## Known limitations

- **Startup time grows with data size:** recovery replays every record (hint files are planned).
- **Data dir lock uses `flock`:** Unix only (macOS/Linux); no Windows build yet.
- **No `DELETE` endpoint, scan, query, or compaction yet.**
- **Keys must fit in RAM:** a Bitcask trait by design. Every live key is held in the keydir.
- **Scans are O(n):** the keydir is a hash map, so prefix scans (and `_find`) walk every key.
- **One global write lock in the service layer:** simple and correct, but writes to different documents serialize.
- Fsync policy and max file size aren't exposed as flags yet.

---

## Contributing workflow

One branch per build step, created from an up-to-date `main`, named `<layer>/<what>`:

```bash
git checkout main && git pull origin main
git checkout -b storage/rotation
# ... work, then:
gofmt -l . && go vet ./... && go test -race ./...
git push -u origin storage/rotation   # open a PR into main
```

Examples used so far: `api/get`, `storage/record-encode`, `storage/put`.

Guidelines (also in `AGENTS.md` for AI coding agents):
- Minimal diffs; no drive-by refactors.
- No new dependencies without discussion (allowed: `github.com/google/uuid`, `github.com/stretchr/testify`).
- Don't change the on-disk record format without discussion, because it breaks existing data files.
- Keep the layer boundaries.
