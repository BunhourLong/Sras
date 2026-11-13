---
name: delegate
description: Delegate bounded Sras implementation, exploration, or review to the configured OpenCode Go and Codex workers (T3 `delegate_task`) while the main session orchestrates.
---

# Sras mixed-model delegation

Read `.claude/run-task.md` for routing and gates. This project skill replaces
the global OpenCode-only roster and its automatic staging step in Sras. Start
each worker with T3's native `delegate_task`; there is no project dispatch
script. Map `role`: `research` for scouts, `implementation` for writers,
`review` for reviewers.

- Target: check `orchestrator_capabilities` first and route per
  `.claude/run-task.md`. OpenCode:
  `target: {providerInstanceId: "opencode", model: "opencode-go/<name>", options: {variant: "max"}}`.
  Codex: `target: {providerInstanceId: "<ChatGPT - Personal id>", model: "gpt-6.1-sol", options: {reasoningEffort: "high"}}`
  (Sol and Astra `high`). Exploration is GLM scouts, never Luna.
- Pair each writer with an independent reviewer: GLM → DeepSeek for routine work,
  Sol → Astra for high-risk storage/concurrency work. Do not silently substitute a model on limits or
  unavailability; record the failure and ask for a routing change unless the
  user already authorized a fallback.
- `mode: "wait"` when this turn needs the result, with `timeoutMs` set for long
  work (60 min max). A timeout does not cancel the child; its completion wakes
  the thread. Do not poll `task_status` on a running task. Jobs that run at
  the same time use `async`; see "Run workers at the same time".
- Pass the brief as text or by absolute path in `task`: a worktree does not
  include the gitignored `.delegate/`, so a relative path fails. Never pass
  secrets, and never raise `runtimeMode` above this thread's.

1. Search before reading and write a self-contained brief in `.delegate/` with
   five headings: Goal, Context, Do, Don't, Done when. Include exact files and
   relevant line references, numbered requirements, expected cases and commands,
   project invariants, and the report format below. Workers have no conversation
   history and must not select a new roadmap task, delegate further, or edit
   global configuration.
2. Use the configured writer. Implementers need a checkout that contains only
   this task's work: use `isolation: "worktree"` when the parent checkout has
   unrelated changes. A T3 worktree starts from the parent commit, so
   uncommitted and gitignored files (`AGENTS.md`, `GOAL.md`, `memory.md`,
   `.delegate/`) are absent: keep a task that depends on them `shared`, or put
   the needed rules and context in the brief. Parallel writers always use
   separate worktrees. No global config edits, dependencies, commits, or
   layer/format changes in a brief without existing authorization. Forward all
   task-specific permissions.
3. Read the diff and execute the brief's checks yourself. Send failures back as
   precise feedback, with the failing command and expected result. At most two
   failed repair rounds before reporting the unresolved issue; no silent model
   fallback. Each repair or review round is a new `delegate_task` with the prior
   decisions and feedback in its brief; an earlier child's memory is not a
   substitute for the brief.
4. Freeze only this task's changes under `.delegate/`, including untracked new
   files, without staging the user's changes. Pass its path and the brief to the
   configured reviewer (DeepSeek for routine work; Astra for high risk, plus an
   optional DeepSeek second opinion started at the same time). Give a
   storage/concurrency reviewer the specific crash/race cases; verify each
   finding yourself. T3 roles are labels, not
   sandboxes: reviewers and scouts must not edit files, so check `git status` in
   the checkout they used before accepting a report.
5. Confirm findings against real cases; add regression tests for bugs. Run the
   configured gates after the last fix, read the final diff, and report results.
   Keep briefs, frozen diffs and reports in `.delegate/`; worker transcripts
   stay in their T3 child threads.

## Run workers at the same time

Jobs that can't conflict start together: send every `delegate_task` in one
response (default `async`), end the turn, and handle each report as its
completion wakes the thread. Several copies of one model at once are fine.
Decide by conflict, not habit: one worker finishing is no reason to wait before
starting an unrelated one. Usually 2–3 workers per round (the user's limit); split by independent
questions or files, never to reach a count, because every worker pays to
rebuild its own context.

- **Read-only jobs run in parallel.** GLM scouts each get one question (where
  is X, what calls Y, what does a spec or another repo say). A question one
  `rg` answers needs no scout. Reviews of the same frozen diff may run side by
  side: for high-risk work, Astra (required) and DeepSeek (optional cheap
  second opinion) together. They share the checkout; check `git status`
  afterwards, because roles are labels, not sandboxes.
- **Writers run in parallel only when nothing is shared:** no file in common
  and no decision left for them to make alike (a name, a signature, an error,
  a helper both need). Make those decisions in the briefs first. Each writer
  gets `isolation: "worktree"` and a self-contained brief (gitignored
  `AGENTS.md`, `GOAL.md`, `memory.md`, `.delegate/` are absent there). Doc
  files most steps touch (`README.md`, `internal/api/openapi.yaml`,
  `AGENTS.md`) belong to one writer or to this session. A worktree also lacks
  uncommitted edits, and Sras never commits a base for it: if the work builds
  on uncommitted changes, or a target file has uncommitted edits, that writer
  stays in the shared checkout and only the other work runs in parallel. Apply
  the diffs one at a time, then run the gates on the combined result.
- **Sequential when one job needs another's output:** writer → its reviewer,
  fix → re-review, two jobs on the same file, a storage change and the service
  code that uses it.
- **Still one roadmap step per task.** Parallel writers split one step; only
  this session picks steps, and only this session updates `GOAL.md` and
  `memory.md`, at close.
- **This session keeps working while workers run** (reading, planning), but
  never polls. It runs tests only when no worker is editing that checkout, and
  edits no file there (mutation checks included) until the round's writers,
  scouts and reviewers have reported, so nobody reads a half-edited or mutated
  file. Mutate via a backup copy and restore it, never `git checkout` on
  unstaged work.

Reports: implementer returns `STATUS: DONE | PARTIAL | BLOCKED`, `CHANGED`,
`RAN`, `RESULT`, and `OPEN`. Scout returns `ANSWER` and `EVIDENCE` with
`path:line`. Reviewer returns `VERDICT: PASS | FINDINGS` and concrete findings
with `path:line`, failing case, and a proposed regression check. Read the
child's final message or `task_status` summary.

A child that asks a question pauses: answer it with `t3_pending_request` on its
`childThreadId`, or cancel and re-brief. Workers have no approval channel:
scouts/reviewers must not edit files, and no report counts until the
orchestrator has read the diff and run the gates.
