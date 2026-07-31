---
name: herdr-edit-constructor-guards-untestable
description: In herdr-edit, a safety guard that lives only in New()/NewSingleFile() can never be proven RED — put it where the comparison is.
metadata:
  type: project
---

In herdr-edit, `internal/app/app_test.go`'s `newTestApp()` builds an `App` with a
struct literal instead of calling `New()` / `NewSingleFile()`. So any guard
implemented purely as constructor-set field state is **invisible to every test in
the package** — the oracle for it passes against the bug, or fails against the fix,
depending on which way you write it, and neither result means anything.

**Why:** hit while building Lane B stage 5. The brief said to floor
`lastDebugSeq` at `time.Now().UnixNano()` "in the App constructors" so a stale
`debug-request.json` cannot launch a debugger at startup. The mandated RED test
(`TestStaleDebugRequestIgnoredAtStartup`) could not exist that way: seeding the
floor in the test is the test doing the production wiring for itself. Resolved by
putting the floor in a package-level `debugRequestFloor` var read *inside*
`consumeDebugRequest` — where the comparison is — with the constructors also
seeding `lastDebugSeq` from that same var, so the two cannot be different numbers.

**How to apply:** when adding a guard to this fork, ask "which single line, if
deleted, makes the oracle go red?" If the answer is a constructor field
assignment, move the guard next to the comparison it protects (a package-level
var, or a lazily-established floor) and have the constructor reference the same
symbol. `app_test.go` is often out of a task's FILES_IN_SCOPE, so you usually
cannot fix `newTestApp` instead. Same reasoning as
[[vzt-agent-protocol]]'s "a gate decided early is not a gate proven".
