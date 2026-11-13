# Sras task configuration

- Plan and next task: `GOAL.md`, section 3; read only the current step and cited sections.
- Session notes: `memory.md`; append outcomes, decisions, caveats, and the brief/role/model needed to re-dispatch.
- Scratch: `.delegate/` (ignored); keep briefs, frozen diffs, reports, and logs here.
- Search: `rg`, then targeted line ranges. No graph service needed.
- Gates: `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...`.
- Focused test: `go test -race ./internal/storage -run '^TestName$'` (choose the affected package).
- Constraints: `AGENTS.md`; ponytail full; no dependencies, record format changes, or layer-boundary changes without the required user decision.
- High risk: record encoding, recovery, compaction, fsync, file lifecycle, lock ordering, and revision conflict checks. Include restart/crash and race cases as applicable.
- Git: preserve existing changes; never auto-stage, commit, push, or merge. Parallel writers use `delegate_task` `isolation: "worktree"`; a T3 worktree starts from the parent commit, so uncommitted and gitignored files (`AGENTS.md`, `GOAL.md`, `memory.md`, `.delegate/`) are absent. Keep tasks that need them in the shared checkout, or put the needed rules and context in the brief.
- Close: update `GOAL.md` per its handoff rules and append `memory.md`; stop after one task. An explicitly requested workflow side task does not advance database steps.

## Worker routing

| Job | Model | Use |
|---|---|---|
| Implementation, tests, fixes | `opencode-go/glm-5.3-flash` | Default writer, `variant: max` |
| Exploration or routine review | `opencode-go/deepseek-v4.1-flash` | Default reader/reviewer, `variant: max` |
| Focused exploration | Codex `gpt-6-luna` | Codex alternative, `reasoningEffort: medium` |
| Complex implementation or tests | Codex `gpt-6.1-sol` | High-risk writer, `reasoningEffort: high` |
| Difficult storage/concurrency review | Codex `gpt-6-astra` | High-risk reviewer, `reasoningEffort: high` |

Use one writer and one independent reviewer per task, rather than running the
entire roster: routine work goes GLM → DeepSeek; high-risk work (the list above)
goes Sol → Astra. Opus stays the orchestrator. Do not silently substitute a model
on limits or unavailability: record the failure and ask for a routing change
unless the user already authorized a fallback. These are routing defaults, not
benchmark results or guarantees of account access.

Workers start only through T3's native `delegate_task` (see the `/delegate`
skill). OpenCode models use the `opencode` provider instance, always at
`variant: max`. Codex models use the "ChatGPT - Personal" provider instance
(`codex_98f8c965-…`; check `orchestrator_capabilities`), not the disabled plain
`codex` instance. No dispatch script, CLI invocation, API integration, or new
service.
