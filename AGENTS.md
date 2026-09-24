# AGENTS.md

Guidance for AI coding agents (Codex, Claude Code, Cursor, Copilot, etc.) working in this repository.

## What this project is
**Sras** is an HTTP document database (NoSQL) written in Go. Clients store JSON documents in named collections over a small REST API. Underneath, documents are persisted by a **Bitcask-style** storage engine:

- **Append-only log files** on disk. Every write (including deletes) is a new record appended to the active file.
- **An in-memory hash index (the keydir)** mapping each live key to the file, offset, and size of its latest value, so a read is one hash lookup plus one disk `ReadAt`.
- **Compaction (merge)** rewrites old immutable files keeping only live records, reclaiming space.
- **Recovery** rebuilds the keydir on startup by replaying the log files.

This is a learning project. **Correctness and clarity come before performance.**

## Stack
- Go 1.22+ (module name `sras`)
- `net/http` with the Go 1.22 method/pattern router (`"GET /db/{collection}/{id}"`, `r.PathValue`). No web framework.
- Standard library only where possible. Allowed extras: `github.com/google/uuid`, `github.com/stretchr/testify` (tests only).
- No CGO, no external database.

## Architecture (strict layering)
Each layer only calls the one directly below it.

```
HTTP API  ->  Service  ->  Query  ->  Storage (Bitcask)  ->  Disk
```

| Layer | Package | Responsibility |
|---|---|---|
| HTTP | `internal/api` | Routing, JSON decode/encode, status codes, middleware. No business logic. |
| Service | `internal/service` | Collections, documents, ID generation, `_rev`, metadata, conflict checks. |
| Query | `internal/query` | Parse filters (`$eq`, `$gt`, `$lt`, `$in`), plan, evaluate against documents. |
| Storage | `internal/storage` | Bitcask engine behind the `Engine` interface. Knows only `[]byte` keys and values. |

Hard rules:
- `internal/storage` never imports JSON, HTTP, or service types.
- `internal/api` never touches storage directly; it depends on a small service interface (see `DocumentService` in `router.go`).
- If a change needs to cross a layer boundary, stop and ask.

## Project layout
```
cmd/server/main.go          # flags, wiring, graceful shutdown
internal/api/               # router.go, handlers.go, middleware.go, errors.go
internal/service/           # documents.go, collections.go
internal/query/             # parser.go, eval.go
internal/storage/
    engine.go               # Engine interface, Options, sentinel errors
    bitcask.go              # Open/Get/Put/Delete/Scan/Close
    datafile.go             # one data file: open/create, readAt, append, sync
    record.go               # record encode/decode + CRC
    keydir.go               # in-memory index
    compaction.go           # Merge
    recovery.go             # rebuild keydir on Open
internal/config/config.go   # defaults for addr, data dir, file size, fsync
data/                       # runtime data dir (gitignored)
```

## Storage engine spec

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
| crc32 (4) | timestamp (8) | keySize (4) | valueSize (4) | key | value |
```
- Header is 20 bytes. CRC32 (IEEE) covers everything after the CRC field.
- Delete = tombstone record: `valueSize = math.MaxUint32`, no value bytes.
- **Do not change this format without asking** — it breaks existing data files.

### Files
- Named `000001.data`, `000002.data`, ... in the data dir.
- Exactly one **active** file (append-only); all older files are **immutable** and opened read-only.
- Rotate to a new active file when size exceeds `MaxFileSize` (default 64 MB). `Put` rotates *before* appending if the record would push the active file past the limit (only when the file is non-empty, so an oversized record gets its own file). `rotate()` fsyncs the old file, reopens it read-only, creates the next ID, and swaps both under `fmu.Lock`. `syncLoop` holds `fmu.RLock` while syncing.

### Keydir
```go
type entry struct {
    fileID    uint32
    valueOff  int64
    valueSize uint32
    timestamp int64
}
map[string]entry   // guarded by sync.RWMutex
```

### Operations
- **Put:** append record to the active file, update keydir, rotate if needed. The keydir stores the value offset: `recordStart + headerSize + len(key)`. The first Put into an empty dir creates `000001.data`; on `Open` the newest file is reopened read-write, older ones read-only.
- **Get:** keydir lookup, then one `ReadAt` covering the whole record (under `fmu.RLock`, so `Close` waits), verify CRC and key.
- **Delete:** append tombstone, remove key from keydir.
- **Scan(prefix):** iterate keydir keys with the prefix. O(n) because the hash index is unordered. Document it, don't hide it.
- **Concurrency:** single writer (`sync.Mutex` on writes), many concurrent readers. Lock order is `wmu` then `fmu`; `Close` takes `wmu` so it waits for an in-flight Put.
- **Durability:** fsync policy `always` | `interval` | `never`, default `interval` (1s). `always` syncs inside Put; `interval` runs a background `syncLoop` that `Close` stops; `Close` always syncs the active file.

### Recovery (on `Open`)
1. List data files in ID order.
2. Replay each record to rebuild the keydir (latest timestamp wins, `>=` so ties go to the later record; tombstone removes).
3. If the active file has an invalid record, truncate at the last valid record and log a warning. An invalid record in an immutable file returns `ErrCorrupt`.
4. Never panic on a bad CRC.
5. `Put` timestamps are monotonic: `max(now, lastTS+1)`, where replay seeds `lastTS`.
6. `Open` holds an exclusive `flock` on `<dir>/LOCK` until `Close` (`ErrLocked` if taken).

### Compaction (`Merge`)
1. Snapshot the immutable file list.
2. Write only live entries (ones the keydir still points at) into new merged files.
3. Atomically swap keydir entries, then delete the old files.
4. Never touch the active file. Reads and writes keep working during a merge.

## Document model
- Storage key: `<collection>/<id>`.
- Collection names: `[a-z0-9_-]{1,64}`. IDs: non-empty, max 256 bytes, no `/` or NUL.
- Value: JSON object with reserved fields `_id`, `_rev`, `_updatedAt`.
- `_rev` is an integer incremented on every write.
- `PUT` with a stale `_rev` returns `409 Conflict` (optimistic concurrency). A missing `_rev` counts as 0, so creating needs no `_rev` and replacing needs the current one. A non-integer `_rev` or non-object body is `400`.
- `_id` is always set from the URL; `_updatedAt` is RFC 3339 UTC. Service `Put` holds one mutex over read-check-write (per-key locks later if needed).

## HTTP API
```
PUT    /db/{collection}/{id}       create or replace a document (201 created / 200 replaced)
GET    /db/{collection}/{id}       read a document
DELETE /db/{collection}/{id}       delete a document (204)
POST   /db/{collection}/_find      body: {"filter": {...}, "limit": 20, "skip": 0}
GET    /healthz                    {"status":"ok"}
POST   /admin/merge                trigger compaction
```
- Error shape: `{"error": {"code": "not_found", "message": "..."}}`
- Error codes: `bad_request`, `not_found`, `conflict`, `internal`.
- Status codes: 200, 201, 204, 400, 404, 409, 500.
- Map service sentinel errors to status codes in one place (`writeServiceError` in `internal/api/errors.go`). Never leak internal error text in a 500.

## Current status
Build order (one step per task):

1. In-memory `Engine` + HTTP CRUD + service layer — **partial**: `GET`/`PUT /db/{collection}/{id}` and `GET /healthz` wired; `DELETE` pending
2. `record.go` + `datafile.go` (encode/decode, CRC) — **done** (`encodeRecord`, `createDatafile`, `append`, `sync`)
3. Bitcask `Put/Get/Delete` with a single data file — **`Get`/`Put` done** (incl. fsync policies), `Delete` pending
4. File rotation + keydir rebuild in `recovery.go` — **done** (`rotate()` in `bitcask.go`; `datafile.records` scanner, `rebuildKeydir`)
5. Tombstones + truncated-tail recovery — **recovery done** (replay honours tombstones; active-file bad tail is truncated + warned; bad immutable file → `ErrCorrupt`); `Delete` pending

Also done with recovery: monotonic Put timestamps (`lastTS`), `flock` on `<dir>/LOCK` (`ErrLocked`, Unix only), `Get` holds `fmu.RLock` across `ReadAt`.
6. Compaction (`Merge`) — pending
7. Query engine and `_find` endpoint — types in `parser.go` only
8. Optional: hint files, secondary indexes, TTL, auth

Unimplemented engine methods return `storage.ErrNotImplemented` and carry a `TODO(build order N)` comment. Do only the step the task asks for.

## Code conventions
- Idiomatic Go: small interfaces, return errors, wrap with `fmt.Errorf("...: %w", err)`.
- Sentinel errors per package (`storage.ErrNotFound`, `storage.ErrCorrupt`, `service.ErrConflict`, ...). Check with `errors.Is`.
- `context.Context` is the first argument in service and API code.
- No global state. Wire dependencies in `cmd/server/main.go`.
- Structured logging with `log/slog`; loggers are injected, default to `slog.Default()` if nil.
- Every exported identifier and package has a doc comment.

## Testing
- Storage: table tests for record encode/decode, put/get/delete, rotation, recovery after a truncated file, and compaction correctness.
- Use `t.TempDir()` for data dirs, never a fixed path.
- Concurrency test: many goroutines doing Put/Get under `-race`.
- API: `httptest` against a real engine in a temp dir.
- Every bug fix gets a regression test.

## Commands
```
go run ./cmd/server -data ./data -addr :8080
gofmt -l .
go vet ./...
go test -race ./...
```
Run `gofmt`, `go vet`, and `go test -race ./...` before finishing any task.

## Working rules for agents
- **Minimal diffs.** Change only what the task needs. No drive-by refactors, renames, or reformatting.
- Before a non-trivial change, state the plan in a few lines, then implement.
- Do not add dependencies without asking.
- Do not change the on-disk record format without asking.
- Keep the layer boundaries. If a change must cross them, stop and ask.
- Do not create extra files (docs, READMEs, scripts) unless requested.
- Never commit the `data/` directory or anything written into it.
- Keep this file current: when a change adds files or changes behaviour described here, update the relevant section (especially **Current status**) in the same task.
- `memory.md` (gitignored, local only) holds running session context: what was done, decisions, open caveats. Read it at the start of a task and append to it at the end.

## Roadmap handoff (`GOAL.md`)
`GOAL.md` (gitignored, local only; skip this section if it doesn't exist) is the step-by-step roadmap. Read its **Next step** section before starting a task. When you finish a task or complete a step (or substep) from it, update `GOAL.md` in the same task, before reporting done:
1. **Mark progress:** change the step's status (⏳ → 🟡 partial / ✅ done) in its heading and in the phase overview; tick completed substeps.
2. **Update "Current state":** move finished items into ✅ Done; remove bugs/risks that are fixed; add any new ones found.
3. **Set the next step:** rewrite section 3 **Next step** to name the next step, its branch name, and *why* it's next (what it unblocks or which bug it fixes). Follow the recommended order unless something discovered during the task changes it, and if so, say why.
4. **Log it:** add a row to **Progress log** (date, step, branch, commit/PR, notes), and bump "Last updated" at the top.
5. **Record decisions:** if the task settled a question in **Cross-cutting design decisions**, write down what was chosen.
6. **Tell the user** in your final message which step is done and which step is next.

**One step per task, then stop.** Work only on the step named in **Next step**, and nothing beyond it. When that step is done and `GOAL.md` is updated, stop and report. Do not start or partly implement the next step, even if it looks small or closely related. The user starts the next step in a new task.

Don't rewrite future phases while doing this; change a later step only if the finished work proved its design wrong, and note why.
