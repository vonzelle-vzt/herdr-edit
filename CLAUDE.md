<!--
  File: CLAUDE.md
  Author: Spicer Matthews <spicer@cloudmanic.com> (upstream)
  Created: 2026-04-29
  Copyright: 2026 Cloudmanic, LLC. (upstream) / 2026 Vonzelle Brown (fork sections)
-->

# CLAUDE.md — herdr-edit

Project-specific guidance for Claude Code. Read this first; it captures
conventions and design decisions that aren't obvious from the code alone.

## What this project is

herdr-edit is a FORK of [cloudmanic/spice-edit](https://github.com/cloudmanic/spice-edit).
Read FORK.md and README.md for what the fork adds and why. Everything below
that is not marked as a fork change is upstream's design, and upstream's
design decisions are to be preserved.

It is an opinionated, **mouse-first** terminal code editor aimed at
SSH-into-tmux workflows, and at living in a herdr pane beside an AI agent. It looks and behaves like a tiny VS Code: file
tree on the left, tabs across the top, syntax-highlighted editor in the
middle, status bar at the bottom. It ships as a single static Go binary
with no CGO.

Users open the action menu (Save, Quit, Show/Hide Sidebar, …) by clicking
the `≡` icon, right-clicking, or double-tapping `Esc`. There are
intentionally **no `Ctrl+` shortcuts** for editor actions — they conflict
with `tmux` and terminal emulators. Don't add them back.

**Every file action also lives in the main ≡ menu.** macOS Terminal +
tmux often swallows Button3 (right-click), so the editor cannot rely on
right-click as the only path to anything. Tree right-click is a redundant
shortcut, not a primary surface — when adding new file-management
features, make sure they're reachable from the main menu first.

## Module / repo

- Module: `github.com/cloudmanic/spice-edit` (import path kept from upstream to keep
  merges clean; the REPO is github.com/vonzelle-vzt/herdr-edit)
- Binary name: `herdr-edit` (Makefile, goreleaser and the brew formula all
  assume this). Deliberately NOT `spiceedit`, so this can sit alongside an
  upstream install without either shadowing the other.
- Brew tap: this same repo, `Formula/` directory (no separate tap repo).
  🔴 The goreleaser `brews.repository` MUST point at vonzelle-vzt/herdr-edit.
  Inherited from upstream it pointed at cloudmanic/spice-edit, so the first
  release here tried to commit a formula into someone else's repository; only
  a GitHub 403 stopped it.

### Never push to upstream
`cloudmanic/spice-edit` is someone else's repo. Two guards, installed by
`scripts/install-guards.sh` (re-run it after a fresh clone — hooks live in
`.git/` and are not cloned):

1. `remote.upstream.pushurl = DISABLED` — stops `git push upstream`.
2. A `pre-push` hook refusing any URL containing `cloudmanic/spice-edit`,
   in either https or ssh spelling.

Note git runs `pre-push` only AFTER connecting, so today the 403 is what you
actually see. The hook is what protects you the day that 403 stops happening —
a permission check is not a design. Contribute upstream by pull request.

## Architecture map

```
main.go                       Entry — parses optional rootDir arg
internal/app/app.go           Event loop, layout, menu modal, splitter, all rendering
internal/editor/buffer.go     Position + Buffer ([]string lines), edit primitives
internal/editor/tab.go        Tab: path, buffer, cursor, anchor, scroll, dirty state
internal/editor/highlight.go  Chroma → []tcell.Style per line
internal/filetree/filetree.go Lazy tree, identity-preserving refresh, hit-test, render
internal/clipboard/clipboard.go OSC 52 to /dev/tty with tmux passthrough wrap
internal/spiceconfig/spiceconfig.go ~/.config/spiceedit/config.json loader (icons mode)
internal/icons/icons.go       Nerd Font detection + per-file glyph mapping
internal/theme/theme.go       Tokyo Night palette + syntax color mapping
internal/version/version.go   const Version = "x.y.z" — single line, CI bumps it

FORK ADDITIONS
internal/lsp/                 LSP client: protocol types, stdio transport, server registry
internal/state/state.go       Publishes active.json so companion tools can follow the cursor
internal/app/diagnostics.go   Diagnostics overlay, drawn AFTER Tab.Render, + status summary
internal/app/startpage.go     The no-tabs-open view: project, branch, changed files
internal/filetree/gitignore.go  git ls-files based filter for the tree
internal/editor/geometry.go   Gutter width + buffer->screen mapping, for the overlay
internal/editor/persist.go    Undo history that survives the process
```

## Conventions

### File headers
Every new source file gets the header block (file name, author, created
date, copyright year). See existing files for the exact format. Keep
copyright year matching the **current year** (2026 right now).

### Comments
- A short doc comment above every function (public **and** private)
  explaining intent. This is a project-wide convention — don't skip it.
- Skip throwaway "what" comments inside functions; favor "why" notes
  for non-obvious decisions.

### Tests — required, not optional
**Every source file gets a corresponding `_test.go` file in the same
package.** New code without tests should not be merged. The bar:

- New exported functions: cover happy path + the obvious failure mode.
- New unexported helpers with non-trivial logic: same.
- Bug fixes: add a test that fails before the fix and passes after.
- Pure data / glue (theme palettes, single-constant files): a smoke
  test that the value is sensible is enough.

Conventions:
- One `_test.go` per source file, in the same package (NOT `_test`),
  so tests can poke unexported helpers directly. Don't split tests
  for one source file across multiple test files.
- Each `Test*` function gets a short doc comment above it explaining
  the behavior it pins down — the same "why over what" rule as
  production code. See `internal/app/fileops_test.go` for the style.
- Use `t.TempDir()` for filesystem state; never write into the repo
  or `/tmp` directly.
- For UI / drawing code that takes a `tcell.Screen`, build one with
  `tcell.NewSimulationScreen("UTF-8")` and assert against
  `scr.GetContents()`.
- Skip a test (`t.Skip`) only when the environment can't satisfy a
  hard requirement (e.g. `/dev/tty` in CI). Don't skip to dodge a
  flaky test — fix it.

Run them locally:
```sh
make test          # go test ./... with race detector
make coverage      # generates coverage.out + an HTML report
```

CI (`.github/workflows/test.yml`) runs `go test ./...` on every push
and every PR; broken tests block merges via the PR's required-checks.

### Commits
- No "Generated with Claude Code" trailers, no Co-Authored-By Claude.
- Don't ask for commit-message approval — commit directly with a good
  message when the user asks you to commit.

## Design patterns to preserve

### `cursorMoved` flag (tab.go)
The cursor only triggers `EnsureVisible` when something actually moved
the cursor. Every cursor mutator sets `t.cursorMoved = true`; `Render`
consumes the flag and clears it. **Do not** call `EnsureVisible`
unconditionally — that re-introduces the "scroll yanks back to cursor
on every tick" bug.

### Scroll clamping with overscroll
`tab.clampScroll(viewH)` allows the last line to scroll roughly to the
middle (`overscroll = max(viewH/2, 3)`). This is intentional — without
it, you can't comfortably read the bottom of a file.

### Custom tcell events for goroutine → main-loop messaging
Background work (auto-scroll during drag, 10s tree refresh) posts custom
events (`autoScrollEvent`, `treeRefreshEvent`) onto the tcell event queue
and the main loop handles them. Don't mutate UI state from goroutines
directly.

### Identity-preserving tree refresh (filetree.go)
`reload` walks the existing children, matches survivors by name, and
keeps their `*Node` pointers (and their `Expanded` state). New entries
get fresh nodes; gone entries are dropped. This is what makes the
10-second auto-refresh feel non-jarring — open folders stay open.

### Three-way external-change reconciliation (app.go)
On each tree-refresh tick, `reconcileOpenTabsWithDisk` checks each open
tab's mtime: clean buffer + changed file → silent reload; dirty buffer
+ changed file → warning; file deleted → set `DiskGone` once.

### Modal layout via `relY` and dynamic `labelFor`
The action menu uses named struct literals with an optional `labelFor`
hook so labels like "Show Sidebar" / "Hide Sidebar" toggle in place.
Dividers are drawn at fixed `relY` offsets — when adding a menu item,
update those offsets and `modalHeight`.

### Sidebar splitter drag
A drag is detected when a press lands at exactly `x == splitterX()`.
Min widths: `minSidebarWidth = 18`, `minEditorAfterDrag = 40`. Don't
let the editor shrink below that.

### Responsive layout (fork) — degrade, never refuse
`minWidth`/`minHeight` are 24x8, not 50x24, because a side panel was otherwise
only usable in a narrow band — one column narrower and the whole editor was
replaced by "Window too small".

The tree **narrows before it hides**. Two helpers own this and nothing else may
duplicate their arithmetic:

- `maxSidebarWidth()` — the widest the explorer block may be right now: the
  editor keeps `minEditorAfterDrag` (40) whenever there is room, otherwise the
  tree pins to `minSidebarWidth` (18) and the editor takes the rest. It is
  `max(18, width-40)`, deliberately **monotonic in width** — an earlier draft
  branched on `minWidth` and made the tree wider at 55 columns than at 58, so
  dragging a pane wider shrank the explorer.
- `sidebarVisible()` — preference AND room, where room is `treeNeeds`
  (`minSidebarWidth + minWidth` = 42), the width below which even a minimum
  tree would push the editor under `minWidth`.

This replaced a flat `sidebarNeeds = 76` that hid the tree outright below 76
columns — which switched the explorer off in exactly the place it earns its
keep: a herdr split beside an agent, where the pane is 60-odd columns because
the agent needs the rest. It rendered a `≡` and the words "click open from the
tree" with no tree in sight.

🔴 `splitterX()` and `resizeSidebar()` MUST go through these, not through
`a.sidebarWidth`. The stored value is a *preference*; on a narrow pane the tree
is drawn narrower. Reading the preference in `splitterX` hit-tests the divider
up to twelve columns from where the user sees it, and a `resizeSidebar` with its
own limit made a 60-column pane draw a 30-wide tree that the first drag snapped
to 20 — the splitter appeared to move the wrong way. Sharing one clamp makes
what is on screen exactly what a drag can reproduce, by construction.

The `sidebarShown` preference is never cleared by the auto-hide, so widening
brings the tree straight back with no keypress.

### Polled call sites, not scattered hooks (fork)
`publishActive()` and `maybeSyncLSP()` are each called from ONE place in
the Run loop. The cursor moves and the buffer mutates from dozens of sites;
a set of hand-placed hooks would miss some, and the symptom would be a
stale panel or stale diagnostics rather than anything that looks like a
bug. Both are cheap when nothing changed.

### The diagnostics overlay draws on top (fork)
`drawDiagnostics()` runs AFTER `tab.Render`, repainting existing runes with
an underline so syntax colours survive. It is a separate pass because the
render path is hot and diagnostics arrive on their own schedule — keeping
them apart means a project with no language server pays nothing.

🔴 LSP counts columns in **UTF-16 code units**; the buffer is **rune**
indexed. Identical for ASCII, so a mistake here survives testing right up
until a line contains an emoji or CJK text. Convert at the boundary
(`lsp.UTF16ToRuneCol` / `lsp.RuneColToUTF16`), never in the middle.

## Build / run

```sh
make run          # go run . in current dir
make build        # build to ./bin/herdr-edit
make build-linux  # cross-compile linux/amd64
make install      # go install to $GOPATH/bin
make tidy         # go mod tidy
make clean        # rm -rf bin
```

There's no `dev server` to run for this project — it's a TUI. To test
UI behavior, build and run it against a real directory.

## Releases (don't break this)

Pushes to `main` trigger `.github/workflows/release.yml`:

1. Reads `internal/version/version.go`.
2. **If that file was edited in the pushed commit**, the version is used
   as-is (manual major/minor bump). **Otherwise** the patch is
   auto-bumped, committed back to main with `[skip ci]`, and pushed.
3. Tags `v<x.y.z>`.
4. GoReleaser cross-compiles, attaches archives to a GitHub Release,
   and writes `Formula/herdr-edit.rb` back into THIS fork (using the
   default `GITHUB_TOKEN` — no PAT). The formula commit also carries
   `[skip ci]` to break the loop.

If you're touching the workflow or `.goreleaser.yml`, make sure both
auto-commits keep their `[skip ci]` markers — without them the workflow
loops forever.

## What NOT to add

- `Ctrl+` editor shortcuts (they fight tmux/terminals — that's the
  whole reason the action menu exists).
- **Third-party dependencies.** The dependency list is tcell, chroma and
  go-gitignore, and that is the whole list. The LSP client is hand-rolled
  against the stdlib specifically to keep it that way — do not swap it for
  a protocol package.
- CGO dependencies. The whole point is one static binary.
- Tree-sitter. We use Chroma intentionally — pure Go, no setup.
- A separate `homebrew-tap` repo. The formula lives here under
  `Formula/` and that's deliberate.
- A plugin system. Upstream said "no config file / dotfile / plugin system"
  and the *plugin* half still holds. The fork does read a small
  `config.json` (icons, `tree.respectGitignore`), because a filter that
  hides files must have an off switch — but that is a settings file, not an
  extension point, and it should stay that small.
