# Sras task configuration

- Plan and next task: `GOAL.md`, section 3; read only the current step and cited sections.
- Session notes: `memory.md`; append outcomes, decisions, caveats, and exact resume commands.
- Scratch: `.delegate/` (ignored); keep briefs, frozen diffs, reports, and logs here.
- Search: `rg`, then targeted line ranges. No graph service needed.
- Gates: `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`.
- Focused test: `go test -race ./internal/storage -run '^TestName$'` (choose the affected package).
- Workflow check: `python3 .claude/skills/delegate/test_dispatch.py` when the dispatcher changes.
- Constraints: `AGENTS.md`; ponytail full; no dependencies, record format changes, or layer-boundary changes without the required user decision.
- High risk: record encoding, recovery, compaction, fsync, file lifecycle, lock ordering, and revision conflict checks. Include restart/crash and race cases as applicable.
- Git: preserve existing changes; never auto-stage, commit, push, or merge. Parallel writers need separate worktrees; copy ignored `AGENTS.md` and the task's cited local context into a fresh worktree before dispatch.
- Close: update `GOAL.md` per its handoff rules and append `memory.md`; stop after one task. An explicitly requested workflow side task does not advance database steps.

## Worker routing

| Job | Model | Use |
|---|---|---|
| Routine implementation | `opencode-go/glm-5.3-flash` | Default writer |
| Exploration or routine review | `opencode-go/deepseek-v4.1-flash` | Default reader/reviewer |
| Focused exploration | `codex/gpt-6-luna` | Codex alternative, medium effort |
| Complex implementation or tests | `codex/gpt-6.1-sol` | High effort |
| Difficult storage/concurrency review | `codex/gpt-6-astra` | High effort; reserve for high-risk work |

Use one writer and one independent reviewer per task, rather than running the
entire roster. Opus stays the orchestrator. Do not silently substitute a model
on limits or unavailability: record the failure and ask for a routing change
unless the user already authorized a fallback. These are routing defaults, not
benchmark results or guarantees of account access.

The project dispatcher reuses `~/.agents/skills/delegate/dispatch.sh` for OpenCode
and invokes the installed ChatGPT-authenticated Codex CLI for Codex workers.
Keep each tool's authentication in that tool. No API integration or new service.
