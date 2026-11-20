// =============================================================================
// File: internal/spiceconfig/spiceconfig.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Package spiceconfig loads the editor's small user-level config from
// ~/.config/spiceedit/config.json. It's separate from customactions on
// purpose: actions.json is a list of shell-out menu entries, config.json
// is editor preferences. Keeping them apart means a malformed actions
// file can't break editor settings and vice-versa.
//
// Schema today is intentionally tiny — one key — but the loader is
// already wrapped in a struct so we can grow new top-level fields
// without breaking older configs:
//
//	{"icons": "auto"}    // default; auto-detect Nerd Fonts on startup
//	{"icons": "on"}      // force-on, even if detection would say no
//	{"icons": "off"}     // force-off, even if a Nerd Font is installed
//	{"completion": {"auto": false}}   // only Esc SPACE opens completions
//	{"inline": {"enabled": true, "model": "qwen2.5-coder:1.5b"}}
//	                                  // AI ghost-text suggestions; OFF by default
//
// The full inline block (fork) is
//
//	{"inline": {"enabled": false, "provider": "ollama", "model": "",
//	            "url": "", "allowRemote": false}}
//
// and every key is optional. internal/app (inlinewire.go) turns it into an
// inline.Config; this package only parses it, so the provider names and the
// endpoint rules live in exactly one place, internal/inline.
//
// The loader is best-effort the same way customactions is: missing
// file → defaults, malformed file → error returned for the app to
// flash, but the editor still starts cleanly.
package spiceconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IconsMode is the user's preference for Nerd Font icons in the file
// tree. "auto" means "use them iff a Nerd Font is installed"; the
// other two values bypass detection entirely.
type IconsMode string

const (
	IconsAuto IconsMode = "auto"
	IconsOn   IconsMode = "on"
	IconsOff  IconsMode = "off"
)

// Config is the resolved, validated form of config.json. Callers get a
// fully-populated Config back from Load — defaults are filled in for
// any field the file omitted, so consumers never need to nil-check.
type Config struct {
	Icons IconsMode

	// RespectGitignore hides git-ignored paths from the file tree. Default ON, matching VS Code,
	// which hides files.exclude entries out of the box — and matching the fuzzy finder, which has
	// always built its index from `git ls-files --exclude-standard`. The tree was the odd one out.
	// A pointer in fileFormat distinguishes "absent" from "explicitly false"; this is the promoted
	// plain bool.
	RespectGitignore bool

	// CompletionAuto opens the completion popup while typing (after a short
	// debounce) instead of only on Esc SPACE. Default ON, like VS Code's
	// editor.quickSuggestions. Same pointer-in-fileFormat promotion as
	// RespectGitignore, so only an explicit false turns it off.
	CompletionAuto bool

	// Inline is the AI inline-suggestion (ghost text) block. Its zero value is
	// OFF, and that is also the default: nothing is constructed and nothing
	// leaves the editor unless the file says "enabled": true.
	Inline Inline
}

// Inline is config.json's "inline" block, trimmed but otherwise as written.
// It deliberately has no API-key field: a key in a dotfile ends up in dotfile
// repositories and screenshots, so remote keys come from the environment.
type Inline struct {
	// Enabled starts suggestions at launch. The ≡ menu can still switch them
	// on for the session, which is the same explicit consent as editing this.
	Enabled bool
	// Provider is lowercased; "" selects the local default (ollama). Unknown
	// names are reported by internal/inline when suggestions are switched on,
	// not here, so the list of names exists once.
	Provider string
	// Model is the model name sent to the provider, e.g. "qwen2.5-coder:1.5b".
	Model string
	// URL is the provider's endpoint; "" means the provider's own default.
	URL string
	// AllowRemote is consent to send buffer text to a provider that is not on
	// this machine. Without it a remote provider is refused outright.
	AllowRemote bool
}

// Defaults returns a Config populated with the values used when no
// config file is present (or every field in it is blank). Centralised
// so tests and the loader can't drift from each other.
func Defaults() Config {
	return Config{Icons: IconsAuto, RespectGitignore: true, CompletionAuto: true}
}

// fileFormat mirrors the on-disk JSON shape. We decode into this and
// then promote into Config so the public type doesn't have to carry
// JSON tags or pointer fields just for "field was absent" detection.
type fileFormat struct {
	Icons      string       `json:"icons,omitempty"`
	Tree       *tree        `json:"tree,omitempty"`
	Completion *completion  `json:"completion,omitempty"`
	Inline     *inlineBlock `json:"inline,omitempty"`
}

// inlineBlock is the on-disk "inline" block. Plain fields, not pointers: every
// default is the zero value (off, local provider, no consent to remote), so an
// absent key and an explicit false mean the same thing.
type inlineBlock struct {
	Enabled     bool   `json:"enabled"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	URL         string `json:"url"`
	AllowRemote bool   `json:"allowRemote"`
}

// tree groups file-explorer settings. Nested rather than flat so future tree options have an
// obvious home, and a pointer so an absent block keeps the default instead of reading as false.
type tree struct {
	RespectGitignore *bool `json:"respectGitignore,omitempty"`
}

// completion groups editor-completion settings, nested for the same reason as tree.
type completion struct {
	Auto *bool `json:"auto,omitempty"`
}

// DefaultPath returns the canonical config-file location:
// $XDG_CONFIG_HOME/spiceedit/config.json, falling back to
// ~/.config/spiceedit/config.json. Returns "" when neither resolves
// — callers should treat that as "use defaults".
func DefaultPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "spiceedit", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "spiceedit", "config.json")
}

// Load reads and parses the config file at path, returning a Config
// with defaults filled in for any missing or blank fields.
//
// Contract:
//   - path == ""              → (Defaults(), nil). Treated as "no
//     config configured".
//   - file doesn't exist      → (Defaults(), nil). Same as above.
//   - file unreadable         → (Defaults(), err). Caller can flash a
//     message; editor keeps running on defaults.
//   - file empty / all-blank  → (Defaults(), nil).
//   - unknown icons value     → (Defaults(), err). We'd rather tell
//     the user their config has a typo than silently fall back to
//     defaults and hide the bug.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return cfg, nil
	}

	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}

	switch IconsMode(strings.ToLower(strings.TrimSpace(ff.Icons))) {
	case "":
		// field omitted — keep default
	case IconsAuto:
		cfg.Icons = IconsAuto
	case IconsOn:
		cfg.Icons = IconsOn
	case IconsOff:
		cfg.Icons = IconsOff
	default:
		return Defaults(), fmt.Errorf(
			"%s: icons must be %q, %q, or %q (got %q)",
			path, IconsAuto, IconsOn, IconsOff, ff.Icons,
		)
	}

	// Absent block or absent key keeps the default; only an explicit false turns it off.
	if ff.Tree != nil && ff.Tree.RespectGitignore != nil {
		cfg.RespectGitignore = *ff.Tree.RespectGitignore
	}
	if ff.Completion != nil && ff.Completion.Auto != nil {
		cfg.CompletionAuto = *ff.Completion.Auto
	}
	if ff.Inline != nil {
		cfg.Inline = Inline{
			Enabled:     ff.Inline.Enabled,
			Provider:    strings.ToLower(strings.TrimSpace(ff.Inline.Provider)),
			Model:       strings.TrimSpace(ff.Inline.Model),
			URL:         strings.TrimSpace(ff.Inline.URL),
			AllowRemote: ff.Inline.AllowRemote,
		}
	}
	return cfg, nil
}
