# Sras task configuration

- Plan and next task: `GOAL.md`, section 3; read only the current step and cited sections.
- Scope: one task is one behaviour or invariant that can finish with tests green. State a short plan (files, invariants, checks, exclusions) and resolve pending design decisions before dependent work. A task the user names takes precedence over the roadmap.
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
follows "Parallel work" below): routine work goes GLM → DeepSeek; high-risk work (the list above)
goes Sol → Astra. Opus stays the orchestrator. Do not silently substitute a model
on limits or unavailability: record the failure and ask for a routing change
unless the user already authorized a fallback. These are routing defaults, not
benchmark results or guarantees of account access. Luna is not used: GLM
scouts do all exploration (user decision, 2026-11-13).

Workers start only through T3's native `delegate_task`, by way of the global
`delikit` skill (`/delikit run-task`, `/delikit split`, `/delikit resume`). This
file is that skill's project configuration and wins where the two differ.
OpenCode models use the `opencode` provider instance, always at
`variant: max`. Codex models use the "ChatGPT - Personal" provider instance
(`codex_98f8c965-…`; check `orchestrator_capabilities`), not the disabled plain
`codex` instance. No dispatch script, CLI invocation, API integration, or new
service.

## Review

Every work item is reviewed; this is the review Delikit leaves to the project.
Freeze only the task's changes under `.delegate/`, untracked new files included,
without staging the user's changes, and give the reviewer that path and the
brief. Routine work: DeepSeek. High risk: Astra, required, with the specific
crash and race cases to check; a DeepSeek second opinion may start at the same
time. Verify each finding against a real case; a confirmed bug gets a
regression test and a fix. Reviewers and scouts must not edit files: T3 roles
are labels, so check `git status` in the checkout they used before accepting a
report. Each repair or review round is a new `delegate_task` whose brief carries
the earlier decisions and feedback; stop after two failed repair rounds.

## Briefs

Save each brief in `.delegate/` and pass it as text or by absolute path (a
worktree lacks `.delegate/`). `role` is `research` for scouts, `implementation`
for writers and `review` for reviewers. A writer needs a checkout holding only
this task's work: use `isolation: "worktree"` when the parent checkout has
unrelated changes, subject to the worktree limits under "Git" above. Workers have no history: give exact files and
lines, numbered requirements, expected cases and commands, and the invariants
that apply. A worker never picks a roadmap task, delegates further or edits
global configuration, and a brief grants no dependency, commit, or layer or
format change without existing authorization; forward every permission the
user did give for the task. Reports: a writer returns
`STATUS: DONE | PARTIAL | BLOCKED`, `CHANGED`, `RAN`, `RESULT` and `OPEN`; a
scout `ANSWER` and `EVIDENCE` with `path:line`; a reviewer `VERDICT: PASS |
FINDINGS` with `path:line`, the failing case and a proposed regression check.

## Parallel work

Jobs that can't conflict start together, several copies of one model included,
usually 2-3 workers per round (the user's limit). Split by independent
questions or files, never to reach a count.

- Read-only: each GLM scout gets one question; a question one `rg` answers
  needs no scout. Reviews of one frozen diff may run side by side.
- Writers: only with no file in common and no shared decision left open (a
  name, a signature, an error, a helper both need); settle those in the briefs.
  Each gets `isolation: "worktree"`. Sras never commits a base for a worktree,
  so work that builds on uncommitted changes, or touches a file with
  uncommitted edits, stays in the shared checkout and only the rest runs in
  parallel. `README.md`, `internal/api/openapi.yaml` and `AGENTS.md` belong to
  one writer or to the main session. Apply the diffs one at a time, then run
  the gates on the combined result.
- Sequential: a writer then its reviewer, a fix then its re-review, two jobs on
  one file, a storage change and the service code that uses it.
- Two roadmap steps may run at once when their files are disjoint. Only the
  main session picks steps, and only it updates `GOAL.md` and `memory.md`, at
  close.
- While workers run, the main session runs tests only when no worker is editing
  that checkout and edits no file there, mutation checks included, until the
  round has reported. Mutate through a backup copy and restore it, never `git
  checkout` on unstaged work.

## Split and resume

A split proposes numbered substeps, each with one outcome, affected files,
acceptance checks, risk and dependencies. It keeps a storage change together
with the recovery logic needed to read it, puts undecided format or interface
questions first, and updates only the requested milestone and the next-step
handoff. On a usage limit, record the
brief path, role, provider and model, options and isolation in `memory.md`; the
next attempt is a fresh `delegate_task`, with no blind retry or model change.
No task stops on an arbitrary tool-call or line-count cutoff.
