// =============================================================================
// File: internal/toolpath/toolpath.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Package toolpath answers one question: where does a developer tool actually
// live on this machine, when PATH cannot be trusted?
//
// 🔴 It exists because this editor's primary environment lies about PATH. herdr's
// server is started by launchd, so it execs a plugin pane with
// PATH=/usr/bin:/bin:/usr/sbin:/sbin -- measured on a live pane, not assumed. So
// exec.LookPath cannot see /opt/homebrew/bin, ~/.local/bin, ~/go/bin,
// ~/.cargo/bin or any npm prefix, and of the nine servers in lsp.DefaultServers
// exactly ONE resolved: clangd, which happens to sit in /usr/bin. Diagnostics,
// hover, completion and go-to-definition were silently absent for every other
// language, and F5 found no debugger at all.
//
// It is its own package rather than a copy in each caller because internal/lsp
// and internal/dap must AGREE about this -- that is CLAUDE.md's stated rule for
// when to share. Two subsystems disagreeing about whether a tool exists is worse
// than either being wrong: the editor would find gopls while the debugger could
// not find dlv sitting in the same directory, and no error would explain it.
// Importing internal/lsp from internal/dap would be a nonsense dependency
// direction (a debug client depending on a language-server client), so the
// shared knowledge moved out to where both can reach it.
package toolpath

import (
	"os"
	"path/filepath"
	"sort"
)

// Dirs returns the directories developer tools install into, in search order.
// Later entries win, so the newest node version beats older ones.
func Dirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "go", "bin"),
			filepath.Join(home, ".cargo", "bin"),
			filepath.Join(home, ".bun", "bin"),
		)
	}
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		dirs = append(dirs, filepath.Join(gopath, "bin"))
	}
	// Every installed node version, newest last so it wins: an npm -g install
	// targets whichever node was active, and we cannot know which that was.
	if home != "" {
		if versions, err := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin")); err == nil {
			sort.Strings(versions)
			dirs = append(dirs, versions...)
		}
	}
	return dirs
}

// Look returns the absolute path of an executable named name in one of Dirs(),
// or "" when there is none. Callers should try exec.LookPath first; this is the
// fallback for the launchd-PATH case described in the package comment.
func Look(name string) string {
	found := ""
	for _, d := range Dirs() {
		p := filepath.Join(d, name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		found = p // keep going: later entries (newer node) deliberately win
	}
	return found
}
