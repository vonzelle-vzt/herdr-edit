# Memory Index

## herdr-edit

- [Concurrent sessions share one checkout](herdr-edit-concurrent-session-collision.md) — 🔴 the branch can change under you mid-task; never `add -A` / `checkout` / `reset`.
- [Constructor-only guards cannot go RED](herdr-edit-constructor-guards-untestable.md) — `newTestApp` skips `New()`; put the guard where the comparison is.
