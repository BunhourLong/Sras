---
name: run-task
description: Complete one Sras roadmap task, resume a recorded task, or split a named milestone into independently verifiable tasks.
---

# One Sras task

Read `.claude/run-task.md`, `AGENTS.md`, and the latest relevant `memory.md`
entry. The target is the user's named task, otherwise `GOAL.md` section 3.
An explicit side task takes precedence over the roadmap. A dispatched worker
follows its brief only.

1. Inspect `git status --short` and affected code. Preserve unrelated changes.
   Choose one behavior or invariant that can finish with tests green. Resolve
   actual pending design decisions before dependent work; existing authorization
   stands. State a short plan with files, invariants, checks, and exclusions.
2. Save a bounded brief in `.delegate/` and follow the project `delegate` skill.
   For a bug, first reproduce it with a failing regression test. For a behavior
   change, name the expected input/output and appropriate tests. Documentation
   and configuration edits need relevant validation, not artificial TDD.
3. Read the resulting diff and run the configured gates yourself. Freeze only
   this task's diff, including new files, and request one independent review.
   Judge concrete findings; a confirmed bug gets a regression test and a fix.
   Re-run affected checks after fixes; report unresolved findings honestly.
4. Update `GOAL.md` and `memory.md` after checks and review pass. Report changes,
   worker models, checks, limitations, and the next task. Leave changes unstaged
   unless the user requested staging. Stop without implementing the next step.

## Split a milestone

Read only that milestone and its dependencies. Preserve its requirements and
propose numbered substeps, each with one outcome, affected files, acceptance
checks, risk, and dependencies. Keep a storage change and the recovery logic
needed to read it together. Put undecided format/interface questions first.
Update only the requested milestone and next-step handoff; do not rewrite future
phases or start implementation in the splitting task.

## Resume

Use the recorded task, brief, frozen diff, and failing command. Check the working
tree before resuming. If a worker hit a usage limit, report it and record the
brief path, role, provider/model, options, and isolation in `memory.md` so the
next attempt is a fresh `delegate_task`; no blind retries or model substitutions.
No arbitrary tool-call or line-count cutoff.
