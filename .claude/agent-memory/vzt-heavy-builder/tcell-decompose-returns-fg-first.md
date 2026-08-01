---
name: tcell-decompose-returns-fg-first
description: tcell Style.Decompose returns (fg, bg, attr) — foreground FIRST. Reading the first value as a background is silently wrong, never a type error, and drawDebugGutter in debug.go currently does exactly that.
metadata:
  type: project
---

`tcell.Style.Decompose()` returns `(fg, bg, attr)`. Every value is a
`tcell.Color`, so binding the first one to a variable named `bg` compiles,
vets, and renders a plausible-looking colour — it is only ever wrong on screen.

**Why:** every overlay in this fork reads the existing cell before repainting it
(`GetContent` → `Decompose` → `SetContent`), so this call is on the path of the
diagnostics underline, the document highlight, the debug gutter and the conflict
tint. Getting it backwards paints the old FOREGROUND as the new background.
Caught while writing `TestConflictTintMarksTheTwoSidesDifferently`: the
assertion read `#C0CAF5` (`theme.Text`) where it expected a tint, which is the
foreground of the cell, not its background.

🔴 **`drawDebugGutter` (internal/app/debug.go, in the `paint` closure) has this
bug live today:** `bg, _, _ := existing.Decompose()` then
`StyleDefault.Background(bg)`. It paints the ● / ○ / ▶ debug glyphs on a
`theme.Muted` slate block instead of the editor background. Cosmetic, low
severity, untouched as of 2026-07-31 because it was outside that task's scope —
verify it is still there before acting on this.

**How to apply:** when writing or reviewing ANY overlay that repaints a cell,
spell the read `_, bg, _ := style.Decompose()` and prefer
`existing.Background(c)` (which mutates only the background) over rebuilding a
style from `StyleDefault`. Assert the colour by scanning a rendered
`SimulationScreen` — the mistake is invisible to the compiler and to `go vet`,
so only a pixel read catches it. Related: [[herdr-edit-constructor-guards-untestable]].
