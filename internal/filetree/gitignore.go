// =============================================================================
// File: internal/filetree/gitignore.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package filetree

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git-ignored paths do not belong in the file tree.
//
// There was an odd asymmetry here: internal/finder already builds its index from
// `git ls-files --cached --others --exclude-standard`, so the fuzzy file picker has always
// respected .gitignore — but the tree read the directory straight off disk and showed .next/,
// dist/, .vercel/, coverage/ and friends. In a JS repo that is most of what you see, and the
// hardcoded shouldHide list could never keep up because every project ignores different things.
//
// The set is built with ONE git invocation for the whole repo rather than `git check-ignore` per
// directory. Directory expansion and the 10-second refresh both reload directories, so a per-
// directory exec would mean N processes every refresh; one call and a map lookup is a fixed cost.
// It is also authoritative in a way a hand-rolled matcher is not: nested .gitignore files, the
// user's global excludesfile and .git/info/exclude are all honoured for free.

// ignoreSet records which paths git considers part of the project. A nil or inactive set allows
// everything, so every failure path — no git binary, not a repo, a git error — degrades to the
// previous behaviour instead of hiding the user's files.
type ignoreSet struct {
	active  bool
	allowed map[string]bool
}

// allows reports whether path should appear in the tree.
func (s *ignoreSet) allows(path string) bool {
	if s == nil || !s.active {
		return true
	}
	return s.allowed[path]
}

// newIgnoreSet asks git for every tracked-or-untracked-but-not-ignored file under root.
//
// git reports files, never directories, so each answer also marks its ancestor directories as
// allowed: a directory earns its place in the tree by containing something worth showing. That
// also gives the right answer for a directory whose contents are entirely ignored — it simply
// never gets marked, and disappears.
func newIgnoreSet(root string) *ignoreSet {
	// -z because paths may contain spaces, quotes, or newlines; without it git quotes them and the
	// strings stop matching what filepath.Join produces.
	out, err := exec.Command("git", "-C", root,
		"ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return &ignoreSet{} // not a repo, or no git — show everything, as before
	}

	allowed := make(map[string]bool, 1024)
	allowed[root] = true
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		rel := string(raw)
		abs := filepath.Join(root, rel)
		allowed[abs] = true
		// Mark every ancestor up to (but not past) the root.
		for dir := filepath.Dir(abs); len(dir) > len(root); dir = filepath.Dir(dir) {
			if allowed[dir] {
				break // this ancestor chain is already marked
			}
			allowed[dir] = true
		}
	}

	// A repo with no files at all is indistinguishable from a git failure by output alone, and
	// staying inactive there is the safer reading: an empty tree helps nobody.
	if len(allowed) <= 1 {
		return &ignoreSet{}
	}
	return &ignoreSet{active: true, allowed: allowed}
}

// RespectGitignore turns the filter on or off for this tree and rebuilds it. Off restores the
// previous behaviour exactly, which is what makes this safe to default on: anyone who genuinely
// wants to browse node_modules has a way back.
//
// The set is mutated THROUGH the pointer rather than replaced. Every Node holds the same pointer,
// so assigning a new one here would leave the whole existing tree filtering against the old set —
// the toggle would appear to do nothing until something happened to rebuild those nodes.
func (t *Tree) RespectGitignore(on bool) {
	if t.ignore == nil {
		t.ignore = &ignoreSet{}
	}
	if !on {
		*t.ignore = ignoreSet{}
		return
	}
	*t.ignore = *newIgnoreSet(t.Root.Path)
}

// IgnoringGitignore reports whether the filter is currently doing anything, so the UI can say so
// rather than leaving the user wondering where a directory went.
func (t *Tree) IgnoringGitignore() bool {
	return t.ignore != nil && t.ignore.active
}

// hiddenByGitignore is a small helper for tests and status text: it answers whether a path is
// being withheld specifically by the git filter, as opposed to the hardcoded shouldHide list.
func (t *Tree) hiddenByGitignore(path string) bool {
	if t.ignore == nil || !t.ignore.active {
		return false
	}
	if shouldHide(filepath.Base(path)) {
		return false
	}
	return !t.ignore.allows(strings.TrimSuffix(path, string(filepath.Separator)))
}
