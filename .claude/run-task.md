# Sras task configuration

- Plan and next task: `GOAL.md`, section 3; read only the current step and cited sections.
- Session notes: `memory.md`; append outcomes, decisions, caveats, and the brief/role/model needed to re-dispatch.
- Scratch: `.delegate/` (ignored); keep briefs, frozen diffs, reports, and logs here.
- Search: `rg`, then targeted line ranges. No graph service needed.
- Gates: `test -z "$(gofmt -l .)" && go vet ./... && go test -race ./... && go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...`.
- Focused test: `go test -race ./internal/storage -run '^TestName$'` (choose the affected package).
- Constraints: `AGENTS.md`; ponytail full; no dependencies, record format changes, or layer-boundary changes without the required user decision.
- High risk: record encoding, recovery, compaction, fsync, file lifecycle, lock ordering, and revision conflict checks. Include restart/crash and race cases as applicable.
- Git: preserve existing changes; never auto-stage, commit, push, or merge. Parallel writers use `delegate_task` `isolation: "worktree"`; a T3 worktree starts from the parent commit, so uncommitted and gitignored files (`AGENTS.md`, `GOAL.md`, `memory.md`, `.delegate/`) are absent. Keep tasks that need them in the shared checkout, or put the needed rules and context in the brief.
- Close: update `GOAL.md` per its handoff rules and append `memory.md`; stop after one task. An explicitly requested workflow side task does not advance database steps.

## Worker routing

| Job | Model | Use |
|---|---|---|
| Exploration (scouts), implementation, tests, fixes | `opencode-go/glm-5.3-flash` | Default scout and writer (cheapest), `variant: max`; several scouts may run at once |
| Routine review | `opencode-go/deepseek-v4.1-flash` | Default reviewer, `variant: max` |
| Complex implementation or tests | Codex `gpt-6.1-sol` | High-risk writer, `reasoningEffort: high` |
| Difficult storage/concurrency review | Codex `gpt-6-astra` | High-risk reviewer, `reasoningEffort: high` |

Each work item gets one writer and its independent reviewer, rather than the
entire roster (a step split across parallel writers, or a second review model,
follows "Run workers at the same time"): routine work goes GLM → DeepSeek; high-risk work (the list above)
goes Sol → Astra. Opus stays the orchestrator. Do not silently substitute a model
on limits or unavailability: record the failure and ask for a routing change
unless the user already authorized a fallback. These are routing defaults, not
benchmark results or guarantees of account access. Luna is not used: GLM
scouts do all exploration (user decision, 2026-11-13).

Workers that can't conflict run at the same time, including several copies of
one model: GLM scouts on separate questions, reviews of one frozen diff next to
each other, writers on disjoint files in separate worktrees. The rules are in
the `delegate` skill's "Run workers at the same time".

Workers start only through T3's native `delegate_task` (see the `/delegate`
skill). OpenCode models use the `opencode` provider instance, always at
`variant: max`. Codex models use the "ChatGPT - Personal" provider instance
(`codex_98f8c965-…`; check `orchestrator_capabilities`), not the disabled plain
`codex` instance. No dispatch script, CLI invocation, API integration, or new
service.
