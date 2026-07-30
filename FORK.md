# herdr-edit — a fork of [cloudmanic/spice-edit](https://github.com/cloudmanic/spice-edit)

SpiceEdit is an excellent mouse-first terminal editor by
[Spicer Matthews](https://github.com/cloudmanic). All the credit for what this is belongs there;
this fork exists only because a handful of the things it deliberately leaves out are the same
handful a VS Code user notices on day one.

Licence is unchanged (MIT). Upstream is tracked as the `upstream` remote and merged in.

## What this fork adds

| | Why it is here |
| --- | --- |
| **LSP diagnostics** — inline squiggles, hover, go-to-definition | The one thing a terminal editor genuinely cannot fake. Everything else a VS Code user misses is a shell command away; an error underlined at the mistake needs a live process that has parsed the project. Verified against real `gopls`. |
| **A file tree that respects `.gitignore`** | Upstream's *finder* already honours it (`git ls-files --exclude-standard`); only the tree did not, so it listed `node_modules/`, `.next/`, `dist/`. On a real Next.js checkout that is 7 of 29 top-level entries. |
| **Find and *replace*** | Upstream find is case-insensitive substring **jump only** — there was no replace at all, in-file or across files. Adds regex, whole-word, case-sensitive, replace, and replace-all as a single undo step. |
| **Auto-closing brackets and quotes** | `InsertRune` inserted exactly one rune. Now pairs close, closers step over, backspace removes both, and a selection gets surrounded — with quotes suppressed after a word character so `don't` does not become `don''t`. |
| **A start page** | With no tab open the editor pane — often the widest thing on screen — rendered two lines of grey text. It now shows the project, branch, and changed files, each clickable. |
| **A layout that degrades instead of refusing** | Below 50×24 upstream replaces everything with "Window too small — please resize". Since the tree is a fixed 30 columns, a side panel was only usable in a narrow band. The tree now narrows toward 18 columns as the pane tightens and only hides below 42, so a 60-column panel beside an agent still shows files; the floors drop to 24×8. |
| **Persistent undo** | History died with the process. |
| **Active-file publishing** | A debounced `{file,line,col,root}` snapshot at `$XDG_STATE_HOME/spiceedit/active.json`. Nothing outside the process could previously know which file was open, which blocks *every* companion panel at once. |

## What it does not change

The design decisions that make SpiceEdit what it is are untouched: no `Ctrl+` editor shortcuts, no
CGO, one static binary, Chroma rather than tree-sitter, mouse-first with the `≡` menu. No new
third-party dependencies were added — the LSP client is hand-rolled against the stdlib for exactly
that reason.

## Going upstream

Three of these are small, self-contained, and match the house style. They are offered upstream
rather than carried here forever:

- **active-file publishing** (`internal/state`) — tiny, optional, and it unlocks companion tooling
  without giving anything a way back into the editor.
- **gitignore-aware tree** (`internal/filetree`) — closes the asymmetry with the finder.
- **inline git blame** — the GitLens signature.

The rest (LSP, replace, auto-close, persistent undo, the responsive layout) are larger or more
opinionated, and stay here.

## Building

```sh
make build      # -> bin/herdr-edit
make test       # go test ./... with the race detector
```

The binary is deliberately named `herdr-edit`, not `spiceedit`, so it can sit alongside an
upstream install without either shadowing the other.
