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

🔴 **Both guards cover `git push`. Neither covers `gh pr create`.** In a repo
GitHub knows is a fork, bare `gh pr create` defaults the **base to the parent**,
so it opens the PR against `cloudmanic/spice-edit` — pushing our branch into
someone else's review queue. Observed: it fails with *"No commits between
cloudmanic:main and vonzelle-vzt:…"*, which reads like a branch problem and is
actually the wrong repository. Always be explicit:

```sh
gh pr create --repo vonzelle-vzt/herdr-edit --base main --head <branch>
```

The `--repo` flag is the guard here; there is no hook that can catch this one,
because no `git` operation takes place.

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
internal/editor/marks.go      Breakpoint/logpoint marks that track buffer edits
internal/search/              Workspace search: a pure-Go scan reusing editor.Matches
internal/app/locationref.go   Parse+jump "path:line:col" from any generated list (Esc e)
internal/app/searchpanel.go   Esc F -> internal/search -> a jumpable synthetic tab
internal/app/problems.go      Diagnostics list + next/prev problem (Esc ; . , and F8)
internal/app/dochighlight.go  Tint other occurrences of the symbol under the cursor
internal/app/breakpoints.go   Breakpoints: conditions, logpoints, persistence, dlv/pdb/gdb export
internal/dap/                 DAP client (delve over a socket, debugpy over stdio, js-debug over a
                              TCP server we dial) — a deliberate SIBLING of internal/lsp's
                              transport, not a shared abstraction
internal/app/debug.go         Debug session, DAP events, stopped marker overlay, status
internal/app/debugview.go     Stepping, call stack, threads, variables, evaluate, debug console
internal/toolpath/            Where developer tools ACTUALLY live when PATH cannot be trusted
internal/langconf/            Per-language editing behaviour (pairs, comments, folding), keyed
                              by language id. data.go is GENERATED — see below
internal/langconf/gen/        The generator: reads VS Code's language-configuration.json files
```

🔴 **`internal/toolpath` is the one utility-shaped package here, and it earns it.**
A herdr pane runs with `PATH=/usr/bin:/bin:/usr/sbin:/sbin` (launchd), so `exec.LookPath` sees none
of `/opt/homebrew/bin`, `~/.local/bin`, `~/go/bin`, `~/.cargo/bin` or any npm prefix. Both
`internal/lsp` and `internal/dap` need this, and they must **agree** — one finding `gopls` while the
other cannot find `dlv` in the same directory is worse than either being wrong, because nothing on
screen would explain it. That is CLAUDE.md's own "share when two copies must AGREE" rule, and
importing `internal/lsp` from `internal/dap` would be a nonsense dependency direction.
`TestOnlyOnePlaceKnowsWhereToolsLive` fails if a third copy appears.

🔴 **`internal/langconf/data.go` is generated, third-party DATA, and carries someone
else's copyright.** It is transcribed by `internal/langconf/gen` from the
`language-configuration.json` files of the built-in language extensions in
`github.com/microsoft/vscode` (MIT). Three rules follow, and none of them are
optional:

1. **Never hand-edit `data.go`.** Change the generator, or run
   `go generate ./internal/langconf`. `TestGeneratedDataMatchesTheSource` re-runs
   the generator and diffs the result byte for byte; a hand-edit is a test failure.
   It skips loudly when VS Code is not installed (CI), and fails when it is
   installed and the data disagrees.
2. 🔴 **MIT requires the copyright notice travel with the data.** `NOTICE` at the
   repo root carries Microsoft's notice, what was taken (data only — no VS Code
   TypeScript, ever), and the VS Code version it came from. If you regenerate
   against a newer VS Code, update `NOTICE`'s version and language count too;
   `langconf.SourceVersion` is the machine-checkable copy of the same claim.
   The **`microsoft/vscode` repository** is MIT; the branded **"Visual Studio Code"
   binary** Microsoft ships is a proprietary build of it. We rely on the former.
   The generator enforces this by taking only extensions whose `package.json`
   declares publisher `vscode` and license `MIT` — which is why `.wat` (vendored
   from `microsoft/vscode-js-debug`, a different repo) is deliberately uncovered.
3. **The extension→language mapping is `lsp.LanguageID`, reused, not copied.**
   `langconf.LanguageOf` is a one-line wrapper and `TestLanguageOfReusesTheLSPMapping`
   fails if a second map appears. Two maps would let the editor and the language
   server disagree about what a file is, and nothing on screen would explain it.

🔴 **`Pair.NotIn` is recorded and NOT enforced — say so, don't quietly imply
otherwise.** VS Code suppresses several pairs inside strings and comments (Rust's
`"`, Go's `'`, most of Python's 22). Honouring that needs scope tracking at the
cursor, which this editor does not have: `HighlightVisible` collapses every Chroma
token into a `tcell.Style` and discards the token type, `Tab.Styles` is populated
for the **viewport only**, and `StyleStale` means the grid describes the buffer
*before* the current keystroke. So the pairs are offered unconditionally and the
data is kept for a caller that could honour it. A quote auto-closing inside a
string is a small annoyance; code that *claims* to suppress it and does not is how
a wrong fix ships. `TestNotInIsRecordedButNotEnforced` is where that claim lives —
update it there, or not at all.

Auto-close in `tab.go` goes through `pairsFor` / `closersFor` / `surroundPairsFor`,
which consult the language and **fall back to the package-level `autoClosePairs`**
for any uncovered file type. That fallback is the whole no-regression story;
`TestUnknownLanguageFallsBackToTheGlobalPairs` is what stops it being dropped.
Those three are reachable — `app.go` calls `InsertRune` on every keystroke.

`editor.ToggleBlockComment` is reachable: `app.go` has a "Toggle block comment"
menu row, which the command palette picks up for free because `paletteCommands`
derives from `menuLayout`. It gets no leader key on purpose — every lowercase
rune is taken or reserved, and `Esc /` belongs to the line-comment toggle, which
is the one people actually reach for.

```
internal/app/lspstatus.go     Which language servers are running, in the status bar
```

⚠️ **Known platform limitation, measured on CI: on linux, debugpy does not deliver
the debuggee's own stdout as DAP `output` events.** The session is otherwise
healthy — it initializes, runs, streams other output events and terminates — but
the program's `print()` never appears, with the same `outputMode: remote` that
works on macOS. So the **debug console will be missing program output on linux**.
`TestLiveDebugpyProgramOutputArrivesAsEvents` asserts the full behaviour on
darwin and the weaker-but-real one elsewhere (output events arrived AND the
program terminated), logging loudly what was not observed. Do not "fix" it by
skipping: a silent skip leaves a linux user with an empty console and nothing to
explain it.

### js-debug: the session is NOT the connection you launched on (fork)

🔴 **js-debug's root session debugs NOTHING.** It is a coordinator: it launches
the program and then sends a **`startDebugging` reverse request** asking us to
open a SECOND session, and the debuggee lives only in that child. There is no
single-session mode — declaring `supportsStartDebuggingRequest: false` does not
stop it, measured. Four things follow, and each one ships broken while looking
fine:

- 🔴 **Breakpoints go on the CHILD.** The root ACCEPTS `setBreakpoints` and
  answers plausibly; nothing binds, and the program runs straight past every
  one of them while the editor reports a healthy session. `Adapter.UsesChildSessions`
  is what routes them, and `TestStartDebuggingOpensAChildAndRoutesBreakpointsToIt`
  asserts on recorded wire traffic which SOCKET carried them — the only evidence
  that exists, since arming the root is not an error at any layer. Confirmed RED
  against the naive version, on both the fake and the real adapter.
- 🔴 **The root still needs its own `configurationDone`.** It will not launch
  the program without one, so `startDebugging` never arrives and F5 hangs until
  the start timeout. "Wait for the child, then configure everything" is the
  obvious order and it deadlocks.
- 🔴 **The child's configuration is sent VERBATIM.** It is just
  `{type, name, __pendingTargetId}`, and that pending-target id is the only
  thing tying the connection to the process the root already spawned. Merging
  our launch keys back in — re-adding `program` — launches a SECOND copy, so the
  user debugs one process while another runs unobserved.
- 🔴 **Every `setBreakpoints` answer is `verified:false`**
  (`"breakpoint.provisionalBreakpoint"`), no line, no later `breakpoint` event
  to upgrade it — **and it then hits**. Read the way delve's answers are read,
  the gutter paints ○ on a working breakpoint and the status bar announces "N
  breakpoint(s) could not be set on an executable line". `Adapter.BreakpointsBindLazily`
  → `boundBreakpoint.Pending` → `WillBind()` is the fix, and the gutter and the
  published panel payload BOTH read `WillBind` so they cannot disagree.

⚠️ **SCOPE: ONE ACTIVE LEAF SESSION, and it is enforced out loud.** A second
`startDebugging` — a browser beside a server, a worker thread, a forked process
— is **refused** with `success:false` and a log line naming the declined
configuration. There is no session tree, no compound configuration, and no
browser+server pair. Dropping it instead would leave the adapter BLOCKED (a
reverse request is a request), and accepting it would replace the session on
screen. **A real Next.js `next dev` will therefore debug only its first target**
— that limit is a deliberate cut, not an oversight, and it is what
`TestSecondStartDebuggingIsRefusedLoudly` pins.

⚠️ **A mid-session re-send answers with an EMPTY array while the breakpoints
stay bound.** `resendBreakpoints` is safe on js-debug (measured: it stops again
after a re-send), but its ANSWER is not a description of state, and
`boundFromAnswers` pairs positionally — so the editor records nothing about
breakpoints that are working. Judge by behaviour, never by re-asking.

**The VS Code copy is NOT usable**, and this one is technical rather than a
licence problem. `ms-vscode.js-debug` declares MIT and would pass the OSI
allow-list — but it ships only `extension.js`, hosted by the extension host,
with no `dapDebugServer.js` anywhere (1.131.0). `LocateJsDebug` probes for the
FILE for exactly that reason. The standalone release tarball is the only route,
we ship no copy of it, and `NOTICE` records that as a separate second entry.

**Two bugs the third adapter exposed in code that already shipped:**

- 🔴 `evaluateArgs.FrameID` was an int with `omitempty`, justified by a comment
  saying frame 0 is not a valid frame id. True of delve, FALSE of js-debug,
  whose innermost frame is `{"id":0}` — so the field was DROPPED and the
  expression was answered globally. It is a `*int` now. The live oracle read
  `Uncaught ReferenceError: a is not defined` for a variable in scope on the
  stopped line; against a name that exists at both scopes it would have answered
  the wrong one with no error at all.
- 🔴 `Stop()` could hang the EDITOR FOREVER. `cmd.Stderr` is a `lineRing`, so
  exec.Cmd makes a pipe and a copying goroutine, and `cmd.Wait` does not return
  until that goroutine sees EOF — which needs every holder of the write end to
  close it, and an adapter's GRANDCHILDREN inherit it. Killing js-debug leaves
  node holding the pipe. `stopDebugSession` runs synchronously on the way out of
  `Run`, so this was the editor refusing to quit. The post-kill wait is bounded
  now. `handleDisconnect` had the same shape: a blocking send onto the launch
  channel, whose one buffered slot is ALREADY FULL because `send` (unlike
  `call`) never removes its pending entry — parking the read goroutine before it
  could post the synthetic `terminated`. Both are non-obvious and neither was
  reachable through delve or debugpy.

🔴 **`ScreenPos` is the overlay contract and it has now shipped wrong TWICE.** First
`dx = gutter + col` (rune index treated as a screen cell); then `contentStart = GutterWidth()` when
`Render` uses `contentX = x + gw + 1` (`tab.go:823`, and `:981` for the wrapped path), which put
*every* overlay one column left of its text on every line. Both times every unit test passed, because
`TestScreenPos_*` **and** the diagnostics overlay test each computed the expected column with the
same formula they were checking. An oracle that restates the arithmetic it checks measures nothing.
`TestScreenPos_MatchesRenderedGlyphs` is the guard: it scans a `SimulationScreen` for where a known
glyph actually landed. Any new geometry or overlay code needs an assertion of that kind — derived
from observed output, never from a second copy of the formula.

🔴 **Every buffer mutation must go through `bufInsert`/`bufDelete`**, which keep `Tab.Marks` in step.
There are **nine** such sites, not the obvious five — `insertAutoClosePair`, both arms of
`surroundSelection` and `deleteEmptyPair` also rewrite the buffer, so breakpoints would stay correct
under ordinary typing and go silently wrong the moment you surrounded a selection with a quote.
`TestAllBufferMutationsGoThroughMarkWrappers` reads `tab.go` and fails on a raw mutation.

## Conventions

### File headers
Every new source file gets the header block (file name, author, created
date, copyright year). See existing files for the exact format. Keep
copyright year matching the **current year** (2026 right now).

🔴 **Put YOUR name in it, not upstream's.** Copying an existing header wholesale
credits Spicer Matthews / Cloudmanic for a file they never wrote. Inherited files
keep their original header; genuinely new files in this fork are
`Author: Vonzelle Brown` / `Copyright: 2026 Vonzelle Brown`. This was got wrong on
four files (`wrap.go`, `wrap_test.go`, `lspactions.go`, `lspactions_test.go`) by
pattern-matching the convention instead of thinking about it.

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

The tree **fits its content, and narrows before it hides**. Three helpers own
this and nothing else may duplicate their arithmetic:

- `autoSidebarWidth()` — what the tree *wants*: `Tree.FitWidth(autoSidebarPercentile)+1`
  (`app.go:1140`; the +1 is the splitter column), floored at `defaultSidebarWidth` so a short-named
  project still looks normal, and capped at `maxAutoSidebarNum/Den` (2/5) of the
  pane so a deep tree with long names cannot push the editor out. This is what
  makes the sidebar **grow** as well as shrink — it previously only ever clamped
  down from a fixed 30, so a 145-column pane still clipped `.pending-shots/`
  with 80 columns going spare.

  `Tree.NaturalWidth()` counts only **expanded** rows, which is what keeps it
  stable: independent of scroll position, so scrolling never resizes the
  sidebar. It shares `rowParts()` with `drawNodeRow` on purpose — the fit has to
  agree with what is painted, and a second copy of the indent/chevron arithmetic
  would drift the first time a glyph changed.

- 🔴 `sidebarUserSized` — set by `resizeSidebar`, i.e. by a splitter drag, and it
  turns auto-fit off for that pane. VS Code never second-guesses a sash you
  dragged. Without this the auto-fit would silently undo every drag on the next
  expand/collapse or resize, which reads as the splitter being broken.
  **Double-clicking the sash clears it** and returns to auto-fit, the way VS Code
  resets a sash — without that escape hatch one drag disabled auto-fit for the
  session and the only cure was closing the pane.

- `maxSidebarWidth()` — the widest the explorer block may be right now: the
  editor keeps `minEditorAfterDrag` (40) whenever there is room, otherwise the
  tree pins to `minSidebarWidth` (18) and the editor takes the rest. It is
  `max(18, width-40)`, deliberately **monotonic in width** — an earlier draft
  branched on `minWidth` and made the tree wider at 55 columns than at 58, so
  dragging a pane wider shrank the explorer.
- `sidebarVisible()` — preference AND room, where room is `treeNeeds`
  (`minSidebarWidth + minWidth` = 42), the width below which even a minimum
  tree would push the editor under `minWidth`.

`FitWidth` takes a **percentile, not the maximum**, and that distinction is the
whole feature. Sizing to the widest row let one 34-character filename in an
expanded folder pin the sidebar to 38% of the pane — worse than the fixed 30 it
replaced, and it made resizing look inert because the panel just sat on its
ceiling. `autoSidebarPercentile` is 85: rare long deep filenames get clipped,
which VS Code does anyway, and the folder names you navigate by fit.

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

### Word wrap is a SEPARATE geometry path (fork)
`Esc z` toggles `Tab.Wrap`, per tab, default off like VS Code. Everything wrapped
lives in `internal/editor/wrap.go` behind that flag, and that isolation is
deliberate: `Render`, `HitTest`, `EnsureVisible`, `clampScroll` and 21 uses of
`ScrollX` all assume **one buffer line == one screen row**. With `Wrap` false not
one line of that original arithmetic runs differently. A cursor that lands on the
wrong character is the worst bug an editor can have, so the new coordinate system
is quarantined rather than threaded through the old one.

- `lineSegments` splits a line into rows, breaking at the last space before the
  limit so words stay whole. A token longer than a row (URL, base64, minified
  line) has no break point and is cut hard. It must **never** emit a zero-width
  segment — that is an infinite loop, not a rendering glitch, and
  `TestWrapNeverLoops` guards it at widths 1–3.
- 🔴 `ScrollSub` counts how many of `ScrollY`'s wrapped rows are above the
  viewport. Without it a single minified line longer than the screen is
  unscrollable: you see its first screenful and cannot reach the rest.
- `segmentOfCol` / `colAtSegmentVisual` are inverses, and
  `TestWrapColumnRoundTrip` sweeps every column of every line at four widths to
  prove it. That round trip is what makes clicks land on the right character.
- Tab stops are measured from the start of each ROW, not the buffer line —
  anchoring to the line would put stops at screen positions that do not exist.
- Continuation rows draw `↪` instead of repeating the line number, which would
  read as several separate lines sharing one number.
- `renderWrapped` is a sibling of `Render`, not a set of branches inside it: the
  two differ in their innermost loop (offset by `ScrollX` vs divide into rows),
  and the unwrapped one is the path that must not regress.

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

### LSP features reachable from the UI (fork)
`Esc h` = hover, `Esc d` = go-to-definition, plus diagnostics. Both requests, the
`Manager` methods, and the `hover`/`definition` client capabilities in
`initialize` existed and were tested for a long time with **no caller at all** —
an audit of every LSP method found the two most-used features after diagnostics
complete and unreachable. If you add another (`references`, `rename`,
`documentSymbol`), remember that implementing the request is the easy half;
grep for a call site before believing it works.

🔴 **This is not an LSP problem — it is a fork-wide pattern, and it has now
happened twice.** `Tab.Replace`, `Tab.ReplaceAll` and `Tab.SetFindOptions` were
complete, unit-tested, and advertised in BOTH README.md and FORK.md as a shipped
headline feature — with **every caller a `_test.go` file**. The find bar drew one
row, had no replace field and no option toggles, so `findOptions()` always
returned the zero value: case-insensitive plain substring, i.e. exactly
upstream's behaviour. A user following the README found nothing there. `FindErr`
was likewise set by the engine and read by nobody, so an uncompilable regex
reported "no results" — indistinguishable from a valid pattern matching nothing.

A green test suite proves the engine works, not that anyone can reach it. Before
believing any feature here is shipped:

```sh
grep -rn "\.YourMethod(" --include="*.go" . | grep -v _test.go   # needs a real call site
```

A row in a feature table is a claim about the UI, and only a non-test caller
substantiates it.

Both run on a goroutine and deliver a **posted event**, like diagnostics. Calling
inline would block `Run`'s `PollEvent` loop on a language server — gopls answers
in milliseconds, but an indexing or wedged server would freeze the editor for the
whole timeout with no way to type.

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

🔴 **Never write that marker in a commit message you want CI to run on.** GitHub
scans the **entire** message, body included — so explaining the marker in prose opts
the commit out. Observed: a commit whose body described the changelog filter produced
**no workflow runs at all**, neither Test nor Release, which reads like a broken
trigger rather than a message that opted out. Describe it ("the CI-skip marker"),
never spell it.

🔴 **A source build goes stale silently, and merging is what makes it stale.** Every
push to `main` auto-tags a release, so `go build -o ~/.local/bin/herdr-edit .` from
last week is now behind — while still being first on `PATH` and reporting no
problem at all. Observed for real: a locally built 0.5.0 running against a tap at
0.5.4, which means a bug you already fixed keeps reproducing.
**Rebuild after every pull.** `herdr-extensions doctor` now compares the binary on
`PATH` against the version in the tapped formula and warns when it is behind.

🔴 **The git identity must stay in its own ungated step.** It used to live inside
`Commit version bump`, which is gated on the auto-bump path (step 2 above), and
`Tag release` borrowed it as a side effect. So hand-editing `version.go` — the
documented way to do a manual major/minor bump — skipped the bump step and left
`git tag -a` with no committer: `fatal: empty ident name`, job dead in 17s,
nothing tagged and nothing shipped. That path had never once run successfully.
A step that configures state must not be gated on a branch other steps depend on.

🔴 **Do not re-add a Pages dispatch.** Upstream deploys a marketing site from
`pages.yml`; this fork has no such workflow, so the inherited
`gh workflow run pages.yml` step failed with *"HTTP 422: Workflow does not have
workflow_dispatch trigger"* and took the job down with it. Releases v0.1.2
through v0.1.6 are all marked FAILED for that reason alone — **after** tagging,
building five platforms, publishing the Release and writing the formula. A red
release that actually shipped is worse than either outcome alone, because a
genuine GoReleaser failure looks identical. Same shape as the `brews.repository`
bug: inherited upstream config pointing at infrastructure this fork lacks.

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
