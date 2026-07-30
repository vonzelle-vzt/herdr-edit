# Repository Guidelines

## Project Structure & Module Organization

SpiceEdit is a Go terminal editor module at `github.com/cloudmanic/spice-edit`; the CLI entry point is `main.go` and the binary is `herdr-edit`. Core packages live under `internal/`: `app` owns the event loop and rendering, `editor` owns buffers/tabs/editing behavior, `filetree` manages the sidebar tree, and supporting packages cover clipboard, formatting, icons, config, theme, finder, and versioning. Tests sit beside source files as `*_test.go`. Release packaging includes `install.sh` and samples under `samples/`. (Upstream's `website/` Hugo site and `Formula/spice-edit.rb` were removed in this fork — they belong to cloudmanic/spice-edit.)

## Build, Test, and Development Commands

- `make run`: run the editor in the current directory with `go run .`.
- `make build`: compile `./bin/spiceedit`.
- `make build-linux`: cross-compile a static `linux/amd64` binary.
- `make test`: run `go test -race ./...`; use before PRs.
- `make test-short`: quick `go test -short ./...` loop while iterating.
- `make coverage`: write `coverage.out` and `coverage.html`.
- `make tidy`: sync `go.mod` and `go.sum`.
- `make site-install`, `make site-dev`, `make site-build`: manage website deps, local Hugo, and production builds.

## Coding Style & Naming Conventions

Use `gofmt`/`go test` defaults and idiomatic Go names: exported identifiers in `CamelCase`, unexported in `camelCase`, package names short and lowercase. New Go source files should follow the existing header block style. Keep short doc comments above functions, including private helpers, explaining intent. Avoid adding `Ctrl+` shortcuts; editor actions must stay reachable from the main `≡` menu because SSH/tmux workflows may swallow shortcuts or right-click events.

## Testing Guidelines

Every non-trivial source file should have a same-package test file, for example `internal/editor/buffer.go` and `internal/editor/buffer_test.go`. Add regression tests for bug fixes and cover happy paths and obvious failures. Use `t.TempDir()` for filesystem state. For drawing tests, use `tcell.NewSimulationScreen("UTF-8")` and assert screen contents.

## Commit & Pull Request Guidelines

Recent commits use concise, imperative summaries, often with PR numbers, such as `Mute dotfiles in tree + per-tab Nerd Font icons (#32)`. Release automation uses `[skip ci]`; preserve that marker when editing generated release commits or workflows. PRs should describe behavior changes, mention tests run, link issues, and include screenshots or terminal captures for UI/website changes.

## Security & Configuration Tips

Format-on-save commands are project config and require trust prompts; do not bypass that flow. Keep generated artifacts (`bin/`, `coverage.out`, `coverage.html`, built CSS) out of normal feature commits unless the release workflow explicitly requires them.
