# Repository Guidelines

## Project Structure & Module Organization

herdr-edit is a Go terminal editor. The module path is still `github.com/cloudmanic/spice-edit` — kept from upstream so merges stay clean — but the repo is `vonzelle-vzt/herdr-edit` and the binary is `herdr-edit`. The CLI entry point is `main.go`.

Packages under `internal/`:

| package | owns |
| --- | --- |
| `app` | the event loop, layout, every modal, and all fork feature wiring |
| `editor` | buffers, tabs, rendering, undo, find, word wrap, marks, merge conflicts |
| `filetree` | the sidebar tree, gitignore filtering, identity-preserving refresh |
| `lsp` | the hand-rolled LSP client: protocol, stdio transport, server registry |
| `dap` | the debug-adapter client — a deliberate SIBLING of `lsp`'s transport, not shared |
| `langconf` | per-language editing behaviour, GENERATED from VS Code's MIT data (see NOTICE) |
| `toolpath` | where developer tools actually live when PATH cannot be trusted |
| `search` | workspace search, reusing `editor.Matches` so there is one matcher |
| `state` | the `active.json` / `open-request.json` / `debug-session.json` contracts |
| `finder` | the background file index and fuzzy scorer |
| supporting | `clipboard`, `customactions`, `format`, `icons`, `spiceconfig`, `theme`, `version` |

Tests sit beside source files as `*_test.go`, in the SAME package. (Upstream's `website/` Hugo site and `Formula/spice-edit.rb` were removed in this fork — they belong to cloudmanic/spice-edit.)

## Build, Test, and Development Commands

- `make run`: run the editor in the current directory with `go run .`.
- `make build`: compile `./bin/herdr-edit`.
- `make build-linux`: cross-compile a static `linux/amd64` binary.
- `make test`: run `go test -race ./...`; use before PRs.
- `make test-short`: quick `go test -short ./...` loop while iterating.
- `make coverage`: write `coverage.out` and `coverage.html`.
- `make tidy`: sync `go.mod` and `go.sum`.
- `make install`: install `./bin/herdr-edit` into `/usr/local/bin`.

There are no `site-*` targets — the Hugo site left with the fork.

🔴 The live oracles are not part of `make test`'s default reachability: they `t.Skip` when their
binary is absent, and a skip reads as a pass. Run them deliberately, with the anti-skip gate on:

```sh
HERDR_REQUIRE_DAP=1 go test ./internal/dap -run TestLive   # delve, debugpy, js-debug, Chrome
go test ./internal/lsp -run TestLive                        # a real gopls
```

## Coding Style & Naming Conventions

Use `gofmt`/`go test` defaults and idiomatic Go names: exported identifiers in `CamelCase`, unexported in `camelCase`, package names short and lowercase. New Go source files should follow the existing header block style. Keep short doc comments above functions, including private helpers, explaining intent. Avoid adding `Ctrl+` shortcuts; editor actions must stay reachable from the main `≡` menu because SSH/tmux workflows may swallow shortcuts or right-click events.

## Testing Guidelines

Every non-trivial source file should have a same-package test file, for example `internal/editor/buffer.go` and `internal/editor/buffer_test.go`. Add regression tests for bug fixes and cover happy paths and obvious failures. Use `t.TempDir()` for filesystem state. For drawing tests, use `tcell.NewSimulationScreen("UTF-8")` and assert screen contents.

## Commit & Pull Request Guidelines

Recent commits use concise, imperative summaries, often with PR numbers, such as `Mute dotfiles in tree + per-tab Nerd Font icons (#32)`. Release automation uses `[skip ci]`; preserve that marker when editing generated release commits or workflows. PRs should describe behavior changes, mention tests run, link issues, and include screenshots or terminal captures for UI/website changes.

## Security & Configuration Tips

Format-on-save commands are project config and require trust prompts; do not bypass that flow. Keep generated artifacts (`bin/`, `coverage.out`, `coverage.html`, built CSS) out of normal feature commits unless the release workflow explicitly requires them.
