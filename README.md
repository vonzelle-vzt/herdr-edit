<!--
  File: README.md
  Copyright: 2026 Cloudmanic, LLC. (upstream) / 2026 Vonzelle Brown (fork)
-->

<p align="center">
  <picture>
    <source srcset="docs/assets/banner.webp" type="image/webp">
    <img src="docs/assets/banner.jpg"
         alt="herdr-edit — VS Code intelligence, terminal speed. A mouse-first terminal editor showing a file explorer, Rust source with an inline diagnostic on line 134, a hover card documenting the width() method, and an AI agent pane suggesting a fix. Feature chips read: LSP Diagnostics, Hover, Go to Definition, Git-aware Tree, Replace, Word Wrap."
         width="100%">
  </picture>
</p>

# herdr-edit

> A mouse-first terminal code editor **with real language intelligence** — inline diagnostics,
> hover, and go-to-definition — built to sit beside an AI agent in a [herdr](https://herdr.dev) pane.

Built on [**cloudmanic/spice-edit**](https://github.com/cloudmanic/spice-edit) by
[Spicer Matthews](https://github.com/cloudmanic) — a genuinely good editor, and the foundation this
stands on. That base gives us the buffer, the renderer, the mouse UI and the event loop; the fork adds
the language intelligence and the responsive layout on top. See
[What's new in this fork](#whats-new-in-this-fork) for exactly which parts are which.

This is the editor half of a two-part stack. The other half is
[**herdr-extensions**](https://github.com/vonzelle-vzt/herdr-extensions) — original work, no upstream —
which turns a herdr session into an IDE: eleven panels, the layout, the keybindings, a live app
preview, screenshot paste, and the installer that puts this editor in place.

```
herdr-extensions   the IDE: panels, layout, install    (100% original)
herdr-edit         the editor those panels drive       ← you are here
                   ├─ internal/lsp     language intelligence   (new here)
                   └─ core editor      buffer, render, mouse    (from spice-edit)
```

Either can be used without the other: the extension falls back to upstream `spiceedit`, and this
editor runs standalone on any terminal.

---

## What it looks like

<p align="center">
  <img src="docs/assets/demo.gif"
       alt="herdr-edit: opening a Go project, walking the file tree, a live gopls diagnostic rendered inline at the end of the line, the command palette, the outline, and the diff view."
       width="100%">
</p>

Recorded with [VHS](https://github.com/charmbracelet/vhs) against a real `gopls` — the diagnostic
is genuine, not a mock. Below is the same editor dumped from its own renderer:

Rendered by the editor itself, not drawn by hand — file tree, gutter, syntax, and an LSP
diagnostic reported inline at the end of the offending line:

```
 EXPLORER                    │  ≡    routes.go ×
 001                         │    1  package api
 ▾ src/                      │    2
   ▾ api/                    │    3  import "net/http"
       routes.go             │    4
   go.mod                    │    5  // Health reports service liveness.
   README.md                 │    6  func Health(w http.ResponseWriter, r *http.Request) {
                             │    7      w.WriteHeader(http.StatusOK)
                             │    8      w.Write([]byte(healthz)) undefined: healthz
                             │    9  }
                             │   10
                             │   11  func Register(mux *http.ServeMux) {
                             │   12      mux.HandleFunc("/healthz", Health)
                             │   13  }
                             │   14
                             │
                             │
                             │
                             │
                             │
                             │
                             │
                             │
                             │
                             │
 Opened routes.go
```

## Why this fork exists

Everything a VS Code user misses in a terminal is a shell command away — file tree, git, search,
formatting, preview — **except one thing**. Squiggles under the actual mistake need a live process
that has parsed your project. You cannot grep your way to "this identifier does not exist".

So this fork's headline is an **LSP client**. It is hand-rolled against the Go standard library, adds
no third-party dependencies, and is verified against real language servers:

```
$ herdr-edit main.go        # with gopls installed
  line 4 col 2  [UndeclaredName] error: undefined: undefinedThing
```

Notably, that is ground nobody else has claimed. herdr's plugin marketplace has 150+ entries and
none ships language intelligence; even Orca — the closest commercial competitor, at 32k+ stars —
embeds standalone Monaco with no LSP at all.

---

## What this fork adds

| Feature | Why it is here |
| --- | --- |
| **LSP diagnostics** — inline underlines, severity colours, message + code in the status bar, and the **message itself rendered dimmed at end-of-line** the way VS Code's Error Lens does | The one thing a terminal editor genuinely cannot fake. An underline tells you *where*; the inline message tells you *what*, without moving the cursor to read a status bar. Only the most severe diagnostic on a line draws, and it truncates rather than wrapping — a two-word fragment of an error is worse than no error. |
| **Hover and go-to-definition** | The other two things you reach for constantly. |
| **A file tree that respects `.gitignore`** | Upstream's *fuzzy finder* already honoured it; only the tree did not, so it listed `node_modules/`, `.next/`, `dist/`. On a real Next.js checkout that is **7 of 29** top-level entries. |
| **Find and *replace*** | Upstream find is case-insensitive substring **jump only** — there was no replace at all. `Esc f` opens the bar; `Tab`, the `›` chevron, or the menu's *Replace in file* expands the replace row. `Alt+c` / `Alt+w` / `Alt+r` toggle case, whole-word and regex — or click `Aa` `ab` `.*`. Enter replaces and advances; Shift+Enter replaces every match **as one undo step**. A pattern that will not compile says so, instead of reporting "no results". |
| **Auto-closing brackets and quotes** | Pairs close, closers step over, backspace removes both, and a selection gets *surrounded*. Quotes are suppressed after a word character so `don't` never becomes `don''t`. |
| **A start page** | With no tab open, the pane showed two lines of grey text. It now shows the project, branch, and changed files — each clickable. |
| **Word wrap** | Upstream has none. `Esc z` reflows long lines to the pane width, breaking on word boundaries, and re-wraps whenever the pane resizes. Off by default, per tab, like VS Code. |
| **A layout that degrades instead of refusing** | Below 50×24 upstream shows *"Window too small — please resize"*. Since the tree is a fixed 30 columns, a side panel was only usable in a narrow band. The tree now **auto-fits**: it grows to show full folder names and narrows toward 18 columns as the pane tightens, hiding only below 42 — a 60-column panel beside an agent still shows files. A splitter drag pins it. Floors drop to **24×8**. |
| **LSP autocomplete** | `textDocument/completion` with a popup under the cursor. Explicitly invoked with `Esc c` rather than firing as you type: an as-you-type popup needs a debounce, a cancellation story and a dismissal rule, and every one of those failure modes shows up as the editor swallowing a keystroke. Only the four keys the popup owns are consumed; anything else dismisses it and is handled normally. |
| **A command palette** | `Esc k`. Fuzzy search over every action, built from the action menu rather than from a list of its own — a second list is a second thing to forget to update. Scored with the same matcher as the file finder, because two notions of "fuzzy" in one program is a bug the user experiences as inconsistency. |
| **Inline git blame** | `Esc b`. Author, coarse relative age and subject, dimmed at end-of-line. Only the cursor's line is blamed — `git blame` on a whole file is linear in history and would run on every scroll. Diagnostics win the end of the line when both want it. |
| **Go to line, select all** | `Esc g`, `Esc a`. `SelectAll` had been complete and unit-tested in `internal/editor` with **zero** non-test callers; only the wiring was missing. |
| **`--open-at`, the reverse contract** | `herdr-edit --open-at path:line[:col]` asks an **already-running** editor to jump there. `active.json` flows editor → panels; this flows panels → editor, which is what turns a read-only review into an edit: the Review panel hands you a line from the agent's diff and you land on it with a language server attached. |
| **A diff view** | `Esc o` opens the active file's diff as a real tab, and pressing it again flips the baseline between your branch's **merge-base** and **HEAD** — "what does this branch change" versus "what have I not committed", which are different questions and the first is the one you ask of an agent's work. Built as a *synthetic tab* rather than a new render mode: word wrap already taught this codebase what a second geometry path costs, so a tab whose buffer happens to hold diff text inherits scrolling, search, selection and mouse hit-testing for free, and refuses to save. |
| **Rename and find-references** | `Esc y` renames a symbol project-wide; `Esc j` lists every use. Rename decodes **both** WorkspaceEdit wire shapes — servers send either `changes` or `documentChanges`, and handling one makes rename silently do nothing against half of them. Edits apply back-to-front per file, because changing text length at one position invalidates every position after it. |
| **Bookmarks** | `Esc m` pins a `file:line`, `Esc '` cycles them. A 401-repo sweep of herdr's marketplace found **no plugin that bookmarks a place in the code** — the "harpoon" ports mark panes and workspaces, which is navigation between windows, not between lines. |
| **The outline, fixes and signature help** | `Esc i` lists the file's symbols, `Esc l` offers code actions, and signature help folds into `Esc h` — it has no separate moment, since you want it while the cursor is inside a call, which is when you would reach for hover anyway. Both pickers reuse the command palette rather than adding a third list widget. Code actions that answer with a *command* rather than edits are dropped: running one needs `workspace/executeCommand`, and offering it would be a menu entry that silently does nothing. |
| **Cursor selection on a diff** | `Esc e` on any row of a diff jumps to that line in the real file. Deleted lines map to where they were removed from. The arithmetic honours the fact that a `-` line exists only in the old file and must not advance the new-file counter — counting rows instead drifts by one per deletion, which is invisible on a small diff and wrong by dozens on a real one. |
| **Persistent undo** | History used to die with the process. |
| **Active-file publishing** | A debounced `{file,line,col,root}` snapshot other tools can read. |

### What it does *not* change

The decisions that make SpiceEdit what it is are untouched, on purpose:

- **No `Ctrl+` editor shortcuts.** They fight tmux and terminal emulators — that is the entire
  reason the `≡` action menu exists.
- **No CGO, one static binary.**
- **Chroma, not tree-sitter.** Pure Go, no setup step.
- **No new third-party dependencies.** The LSP client is stdlib-only for exactly this reason.

---

## Keys

`Esc` is the leader. Press it, then a letter, within about half a second. Pressing `Esc` **twice**
opens the `≡` action menu, which lists every action here plus the file operations — the menu is the
primary surface, because macOS Terminal and tmux frequently swallow right-click.

| Key | Action | |
| --- | --- | --- |
| `Esc` `Esc` | Action menu | also: click `≡`, or right-click |
| `Esc` `s` | Save | |
| `Esc` `u` / `r` | Undo / redo | history survives closing the editor |
| `Esc` `n` | New file | relative to the active folder; creates intermediate dirs |
| `Esc` `w` / `q` | Close tab / quit | |
| `Esc` `t` | Show/hide the file tree | |
| `Esc` `/` | Toggle line comment | marker chosen by file type |
| `Esc` `f` | **Find** in file | |
| `Esc` `p` | **Find file** in project | fuzzy, background-indexed |
| `Esc` `h` | **Hover** — types and docs at the cursor | needs a language server |
| `Esc` `d` | **Go to definition** | needs a language server |
| `Esc` `z` | Toggle **word wrap** | per tab, off by default |
| `Esc` `space` | **Complete at cursor** | LSP autocomplete; explicitly invoked, never as-you-type. `space` mirrors VS Code's Ctrl+Space, and keeps `c` free for the terminal's own clipboard |
| `Esc` `k` | **Command palette** | fuzzy search over every action |
| `Esc` `g` | **Go to line** | accepts `N` or `N:C` |
| `Esc` `a` | **Select all** | |
| `Esc` `b` | Toggle **inline git blame** | author and commit on the cursor's line |
| `Esc` `o` | **Open changes** — the file's diff as a tab | press again to flip merge-base ⟷ HEAD |
| `Esc` `j` | **Find references** | every use of the symbol, as a list you can open from |
| `Esc` `i` | **Go to symbol** — the file's outline | nested symbols indented |
| `Esc` `l` | **Fix at cursor** — code actions | the lightbulb, as in VS Code |
| `Esc` `e` | **Jump to source line** from a diff | put the cursor on any diff row |
| `Esc` `y` | **Rename symbol** | project-wide, applied across files on disk |
| `Esc` `m` | **Toggle bookmark** on this line | |
| `Esc` `'` | **Next bookmark** | cycles, wrapping |

Deliberately unbound: `c` / `x` / `v`, because the terminal's own copy and paste already own that
path; and rename / delete / revert, which are destructive enough to want the menu's confirm dialog.

### Inside the find bar

The bar is one row, or two when the replace field is expanded. Expand it with `Tab`, by clicking the
`›` chevron on the left, or from the menu's *Replace in file*.

| Key | In the find field | In the replace field |
| --- | --- | --- |
| `Enter` | next match | **replace** and advance |
| `Shift`+`Enter` | previous match | **replace all**, as one undo step |
| `Tab` | expand / focus replace | back to the find field |
| `Alt`+`c` / `w` / `r` | case-sensitive · whole-word · regex | same |
| `Esc` | close the bar and clear highlights | same |

Every toggle is also a **click target** — `Aa`, `ab`, `.*` in the bar, and `[Replace]` / `[All]` on
the replace row — so terminals that never deliver `Alt` lose nothing. Regex is Go's RE2: it cannot
backtrack, so a hostile pattern cannot hang the editor. A pattern that will not compile says
`bad pattern` rather than reporting "no results", which would be indistinguishable from a valid
pattern that genuinely matches nothing.

### Mouse

The editor is mouse-first and expects it to work over SSH. Click to place the cursor, drag to select
(it auto-scrolls past the edge), wheel to scroll, `Shift`+wheel to scroll horizontally. Click a file
in the tree to open it, double-click a folder to fold it, and drag the splitter to size the tree —
a drag pins your width, and double-clicking the splitter hands it back to auto-fit.

---

## What's new in this fork

[FORK.md](FORK.md) explains *why* each addition exists. This is the ownership map — which code is
new here and which came from upstream — measured from git history rather than estimated:

| | lines | files |
| --- | --- | --- |
| **New in this fork** | 5,678 | 23 |
| From upstream spice-edit | 26,599 | 64 |

Two packages exist only here, and they are the ones the headline rests on:

| Package | Lines | What it is |
| --- | --- | --- |
| **`internal/lsp`** | 1,740 | The whole LSP client — protocol types, stdio transport, server registry, UTF-16 ↔ rune conversion. Hand-rolled on the standard library, no new dependencies. |
| **`internal/state`** | 323 | The `active.json` contract: publishes `{file,line,col,root}` so companion tools can follow the cursor. Every herdr-extensions panel reads it. |

And inside packages shared with upstream, these files are new:

| Area | Files |
| --- | --- |
| Diagnostics overlay + status summary | `internal/app/diagnostics.go` |
| Hover / go-to-definition wiring | `internal/app/lspactions.go` |
| Word wrap (a separate geometry path) | `internal/editor/wrap.go` |
| Start page instead of "No file open" | `internal/app/startpage.go` |
| Undo history that survives the process | `internal/editor/persist.go` |
| `.gitignore`-aware file tree | `internal/filetree/gitignore.go` |
| Gutter/screen mapping for the overlay | `internal/editor/geometry.go` |

The remaining 82% — buffer, tab, renderer, event loop, modals, mouse handling, file operations,
finder, formatting, icons, theme — is upstream's, and stays credited as such. spice-edit is MIT and
this fork keeps its copyright notice, which is both required and correct: inherited files keep their
original headers, and files new here carry ours.

## Install

**Most people should not install this directly.**
[herdr-extensions](https://github.com/vonzelle-vzt/herdr-extensions) installs it for you, along with
the panels, the keybindings and the rest of the IDE:

```sh
brew tap vonzelle-vzt/herdr-extensions https://github.com/vonzelle-vzt/herdr-extensions
brew install vonzelle-vzt/herdr-extensions/herdr-extensions
herdr-extensions install        # brings this editor with it
```

**Standalone**, if you want the editor on its own:

```sh
brew tap vonzelle-vzt/herdr-edit https://github.com/vonzelle-vzt/herdr-edit
brew install vonzelle-vzt/herdr-edit/herdr-edit
herdr-edit --version
```

**From source** — Go 1.24+ (per `go.mod`), no CGO, no third-party build steps:

```sh
git clone https://github.com/vonzelle-vzt/herdr-edit
cd herdr-edit
make build          # -> bin/herdr-edit
make test           # go test -race ./...   (14 packages)
make install        # -> $GOPATH/bin
```

⚠️ **Rebuild after every pull.** A source build never updates itself, and every push to
`main` here auto-tags a release — so a binary built last week is behind while still being
first on your `PATH` and reporting nothing wrong. `herdr-extensions doctor` compares the
binary on `PATH` against the tap and warns when it has fallen behind.

The binary is deliberately named `herdr-edit`, **not** `spiceedit`, so it can sit alongside an
upstream install without either shadowing the other. If you build from source *and* install via brew,
note that whichever of `~/.local/bin` or `/opt/homebrew/bin` comes first on your `PATH` wins — and
herdr-extensions resolves the editor with `which`, so it follows the same order you do.

Verified against a real install: `brew fetch` checksum passes, `make build` produces a working
8.9 MB binary, and `make test` is green across all 14 packages.

---

## Language servers

Servers start **lazily**, on the first file of a matching language, and one that fails to start is
never retried. A project with no server installed spawns nothing and costs nothing.

| Language | Server it looks for |
| --- | --- |
| TypeScript / JavaScript | `vtsls`, then `typescript-language-server` |
| Python | `basedpyright-langserver`, then `pyright-langserver` |
| Go | `gopls` |
| Rust | `rust-analyzer` |
| CSS / SCSS / HTML | `tailwindcss-language-server` |
| JSON | `vscode-json-language-server` |
| YAML | `yaml-language-server` |
| Bash | `bash-language-server` |
| C / C++ | `clangd` |

**`node_modules/.bin` is searched before `PATH`**, so a repo's pinned server — matching that repo's
own TypeScript version — wins over whatever happens to be installed globally.

Nothing here blocks the editor. Answers arrive on background goroutines and are marshalled onto the
event loop, so a hung or crashed server costs you the absence of squiggles and nothing else. Server
complaints surface on the status line, because a misconfigured server that says nothing is the most
common way an LSP setup appears to "just not work".

---

## Configuration

`~/.config/spiceedit/config.json` (or `$XDG_CONFIG_HOME/spiceedit/`):

```jsonc
{
  "icons": "auto",                       // "auto" | "on" | "off"
  "tree": { "respectGitignore": true }   // default true; false restores upstream behaviour
}
```

Unknown keys are ignored, so the file is safe to grow.

See [`UPSTREAM-README.md`](UPSTREAM-README.md) for custom actions (`actions.json`) and format-on-save
(`.spiceedit/format.json`) — both inherited unchanged.

---

## The active-file contract

The editor publishes what you are looking at, so companion panels can follow along:

```jsonc
// $XDG_STATE_HOME/spiceedit/active.json   (default ~/.local/state/spiceedit/active.json)
{ "file": "/abs/path.ts", "line": 42, "col": 7, "root": "/abs/repo", "ts": 1785372000000 }
```

`line` and `col` are **1-based**. `file` is empty when no tab is open.

This is *not* an IPC channel and nothing can talk back — it is one debounced, best-effort,
atomically-renamed snapshot. It exists because nothing outside the process could previously know
which file was open, and that single gap blocks *every* companion view at once: blame for this file,
problems for this file, run the tests for this file. herdr-extensions' panels all read it.

Design notes, since they are easy to get wrong:

- **Debounced at 150 ms.** Cursor movement fires on every keystroke and every mouse drag; writing
  eagerly would mean thousands of syscalls during a scroll.
- **Written via temp file + rename**, so a reader never observes a half-written file.
- **Published from one call site** in the draw loop rather than from each of the ~40 places that
  move the cursor. One polled site cannot miss a case; hand-placed hooks would.

---

## Gotchas worth knowing

**LSP counts UTF-16 code units; this editor counts runes.** They are identical for ASCII, which is
exactly why getting it wrong survives testing — it only breaks once a line contains an emoji or CJK
text, and then every column past it is off. Conversion happens at the protocol boundary.

**A diagnostic `code` may be a string *or* a number.** The spec allows both, and decoding into a
concrete type makes every server that chose the other one fail to parse — dropping the *whole*
diagnostics payload, not just the code field.

**A publish replaces, it never merges.** Servers send the complete list for a document every time,
including an empty list once everything is fixed. Merging would leave repaired problems underlined
forever.

**Icons need a Nerd Font *and a terminal configured to use it*.** The editor detects fonts on disk;
it cannot know what font your terminal renders with. A tree full of question marks is almost always
this. `herdr-extensions doctor` diagnoses it by name.

**A tested engine is not a shipped feature — and this fork has been caught by it twice.** `hover`
and `definition` were complete, unit-tested and advertised in the LSP `initialize` handshake with
zero call sites for months. Find-and-*replace* then repeated it exactly: `Tab.Replace`,
`ReplaceAll` and `SetFindOptions` were fully tested and named as a headline feature in this README
*and* in FORK.md, while every caller was a `_test.go` file. The bar drew a single row with no
replace field, so `findOptions()` always returned the zero value and the editor silently behaved
just like upstream. Before believing a feature here works:

```sh
grep -rn "\.YourMethod(" --include="*.go" . | grep -v _test.go
```

A green suite proves the engine is correct, not that a user can reach it.

---

## Development

```sh
make build      # -> bin/herdr-edit
make test       # go test ./... with the race detector
make coverage   # coverage.out + an HTML report
```

The test suite covers 14 packages, and `internal/lsp/live_gopls_test.go` drives **every** LSP
request against a **real** `gopls` — hover, definition, completion, references, rename,
documentSymbol, signatureHelp and codeAction. That file exists because every other test here feeds
hand-written JSON to a decoder, which proves the decoder handles the shape it was handed and
nothing about whether a real server sends it. This fork shipped a "complete, tested" LSP feature
nobody could reach three separate times; a green decoder test is exactly the evidence that allowed
it. Install the server with `go install golang.org/x/tools/gopls@latest`; the file skips when it is
absent, so a fresh clone stays green. Conventions (file headers, a `_test.go` beside every source file,
`tcell.NewSimulationScreen` for drawing tests) are documented in [`CLAUDE.md`](CLAUDE.md) and
inherited from upstream — please keep to them.

The Go module path stays `github.com/cloudmanic/spice-edit` on purpose: changing it would make every
upstream merge a conflict for no benefit, since the repo and binary names are what users actually see.

---

## Going upstream

Three of these changes are small, self-contained, and match the house style, so they are offered
upstream rather than carried here forever:

1. **Active-file publishing** (`internal/state`) — tiny, optional, and it unlocks companion tooling
   without giving anything a way back into the editor.
2. **gitignore-aware tree** (`internal/filetree`) — closes the asymmetry with the finder.
3. **Inline git blame** — the GitLens signature.

The rest (LSP, replace, auto-close, persistent undo, the responsive layout) are larger or more
opinionated and stay here. See [`FORK.md`](FORK.md).

---

## License

MIT — see [`LICENSE`](LICENSE).

Copyright © 2026 Cloudmanic, LLC. (everything this was forked from)
Copyright © 2026 Vonzelle Brown (fork modifications)
