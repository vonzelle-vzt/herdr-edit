<!--
  File: README.md
  Copyright: 2026 Cloudmanic, LLC. (upstream) / 2026 Vonzelle Brown (fork)
-->

# herdr-edit

> A mouse-first terminal code editor **with real language intelligence** — inline diagnostics,
> hover, and go-to-definition — built to sit beside an AI agent in a [herdr](https://herdr.dev) pane.

A fork of [**cloudmanic/spice-edit**](https://github.com/cloudmanic/spice-edit) by
[Spicer Matthews](https://github.com/cloudmanic), which is where the credit for this belongs.
SpiceEdit is a genuinely good editor; this fork exists because a handful of the things it
deliberately leaves out are the same handful a VS Code user notices on day one.

It pairs with [**herdr-extensions**](https://github.com/vonzelle-vzt/herdr-extensions), which
installs it, wires up the panels around it, and keeps them out of herdr's own keybindings.

```
herdr-extensions   the installer, the panels, the skin
herdr-edit         the editor those panels drive     ← you are here
```

---

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
| **LSP diagnostics** — inline underlines, severity colours, message + code in the status bar | The one thing a terminal editor genuinely cannot fake. |
| **Hover and go-to-definition** | The other two things you reach for constantly. |
| **A file tree that respects `.gitignore`** | Upstream's *fuzzy finder* already honoured it; only the tree did not, so it listed `node_modules/`, `.next/`, `dist/`. On a real Next.js checkout that is **7 of 29** top-level entries. |
| **Find and *replace*** | Upstream find is case-insensitive substring **jump only** — there was no replace at all. Adds regex, whole-word, case-sensitive, replace, and replace-all **as one undo step**. |
| **Auto-closing brackets and quotes** | Pairs close, closers step over, backspace removes both, and a selection gets *surrounded*. Quotes are suppressed after a word character so `don't` never becomes `don''t`. |
| **A start page** | With no tab open, the pane showed two lines of grey text. It now shows the project, branch, and changed files — each clickable. |
| **A layout that degrades instead of refusing** | Below 50×24 upstream shows *"Window too small — please resize"*. Since the tree is a fixed 30 columns, a side panel was only usable in a narrow band. The tree now **narrows** toward 18 columns as the pane tightens and only hides below 42 — a 60-column panel beside an agent still shows files. Floors drop to **24×8**. |
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

## Install

```sh
brew tap vonzelle-vzt/herdr-edit https://github.com/vonzelle-vzt/herdr-edit
brew install vonzelle-vzt/herdr-edit/herdr-edit
```

Or build it:

```sh
git clone https://github.com/vonzelle-vzt/herdr-edit
cd herdr-edit && make build      # -> bin/herdr-edit
```

The binary is deliberately named `herdr-edit`, **not** `spiceedit`, so it can sit alongside an
upstream install without either shadowing the other.

Most people should install [herdr-extensions](https://github.com/vonzelle-vzt/herdr-extensions)
instead — one command, and it sets this up along with the panels and keybindings.

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

---

## Development

```sh
make build      # -> bin/herdr-edit
make test       # go test ./... with the race detector
make coverage   # coverage.out + an HTML report
```

The test suite covers 14 packages. Conventions (file headers, a `_test.go` beside every source file,
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
