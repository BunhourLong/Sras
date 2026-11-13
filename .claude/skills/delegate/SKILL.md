---
name: delegate
description: Delegate bounded Sras implementation, exploration, or review to the configured OpenCode Go and Codex CLI workers while the main session orchestrates.
---

# Sras mixed-model delegation

Read `.claude/run-task.md` for routing and gates. This project skill replaces
the global OpenCode-only roster and its automatic staging step in Sras.
Use `sh .claude/skills/delegate/dispatch.sh ROLE MODEL BRIEF [SESSION]` from
the checkout root. Roles: `scout`, `implementer`, `reviewer`.

1. Search before reading and write a self-contained brief in `.delegate/` with
   five headings: Goal, Context, Do, Don't, Done when. Include exact files and
   relevant line references, numbered requirements, expected cases and commands,
   project invariants, and the report format below. Workers have no conversation
   history and must not select a new roadmap task or delegate further.
2. Use the configured writer. Implementers require a clean checkout: use an
   isolated worktree when this checkout contains unrelated work, or ask before
   changing those files. For task-owned repair rounds or a user-authorized dirty
   baseline, explicitly set `DELEGATE_ALLOW_DIRTY=1`; never use it to bypass an
   unresolved ownership question. Parallel writers always use separate worktrees. No
   global config edits, dependencies, commits, or layer/format changes in a brief
   without existing authorization. Forward all task-specific permissions.
3. Read the diff and execute the brief's checks yourself. Send failures back as
   precise feedback, with the failing command and expected result. At most two
   failed repair rounds before reporting the unresolved issue; no silent model
   fallback. OpenCode supports the returned session ID. Codex dispatches start
   fresh; put prior decisions and feedback in the next brief.
4. Freeze only this task's changes under `.delegate/`, including untracked new
   files, without staging the user's changes. Pass its path and the brief to a
   different model for one review. Low risk: DeepSeek by default; difficult
   storage/concurrency work: Astra. Reviewers and scouts must not edit files.
5. Confirm findings against real cases; add regression tests for bugs. Run the
   configured gates after the last fix, read the final diff, and report results.
   Keep transcripts in `.delegate/`; read final reports instead of whole logs.

Reports: implementer returns `STATUS: DONE | PARTIAL | BLOCKED`, `CHANGED`,
`RAN`, `RESULT`, and `OPEN`. Scout returns `ANSWER` and `EVIDENCE` with
`path:line`. Reviewer returns `VERDICT: PASS | FINDINGS` and concrete findings
with `path:line`, failing case, and a proposed regression check. Existing
OpenCode role reports use `OUTPUT`/`NOT DONE`; accept those equivalents.

```sh
sh .claude/skills/delegate/dispatch.sh scout codex/gpt-6-luna .delegate/scout.md
sh .claude/skills/delegate/dispatch.sh implementer glm-5.3-flash .delegate/implement.md
sh .claude/skills/delegate/dispatch.sh reviewer codex/gpt-6-astra .delegate/review.md
```

For OpenCode, the installed global dispatcher, its `delegate/*` roles, `jq`,
and `perl` are required. Codex needs the installed CLI and its own login.
Codex scouts/reviewers use a read-only sandbox; implementers use workspace-write.
They cannot obtain new approvals in a background run. No permission bypass.
