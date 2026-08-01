// =============================================================================
// File: internal/filetree/filetree.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Package filetree implements the left-hand sidebar's file explorer. It is a
// lazy directory tree: children are only read from disk when their parent is
// expanded, so opening the editor on a huge repo is still instant. The tree
// also keeps a flat list of "currently visible" rows so that hit-testing a
// click against rendered rows is O(1).
package filetree

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/icons"
	"github.com/cloudmanic/spice-edit/internal/theme"
)

// Node is a single entry in the file tree. Directories also carry their
// children (loaded lazily on first expansion); files carry only their path.
type Node struct {
	Path     string
	Name     string
	IsDir    bool
	Expanded bool
	Loaded   bool
	Children []*Node

	// ign is the git-ignore filter, shared by pointer with the whole tree and inherited by every
	// child at creation. It lives on Node rather than Tree because reload() is a Node method
	// reached from both loadChildren and refreshNode, and threading it through either a package
	// global or a Tree back-pointer would be worse.
	ign *ignoreSet
}

// GitChangeKind describes the strongest git status a tree row should show.
type GitChangeKind int

const (
	GitChangeNone GitChangeKind = iota
	GitChangeModified
	GitChangeAdded
	GitChangeDeleted
	GitChangeRenamed
	GitChangeMixed
	// GitChangeConflict is an UNMERGED path — a merge, rebase, cherry-pick or
	// revert left conflict markers in it.
	//
	// 🔴 Appended after GitChangeMixed on purpose. These are ordinals, and moving
	// an existing one silently reinterprets anything that stored or published a
	// number rather than a name.
	GitChangeConflict
)

// Tree owns the root node and the most recently rendered flat list of
// visible rows. Click hit-testing maps a screen row index back to the Node
// drawn at that row.
type Tree struct {
	Root    *Node
	visible []*Node // index = screen row in the list area; nil for blank rows.
	ScrollY int

	// ignore withholds git-ignored paths. Shared by pointer with every Node, so toggling it
	// reshapes the whole tree on the next reload without rebuilding anything.
	ignore *ignoreSet

	// ActiveFolder is the absolute path of the folder the user is
	// currently "working in" — the default target for actions like New
	// File. The Render() method bolds the matching row so the choice is
	// always visible. The app updates this whenever the user clicks a
	// tree node or opens a file.
	ActiveFolder string
	ActiveFile   string

	// DirtyFiles and DirtyFolders carry the project's git status — both
	// indexed by absolute path. Files in DirtyFiles render in the theme's
	// Modified color; folders in DirtyFolders do the same so a collapsed
	// branch still signals there's a change inside. Both maps are nil
	// when the project isn't a git repo or when git status hasn't been
	// loaded yet, and the renderer treats nil as "everything clean".
	DirtyFiles   map[string]GitChangeKind
	DirtyFolders map[string]GitChangeKind

	// IconsEnabled toggles the Nerd Font glyph that prefixes each row.
	// Set by App.loadSpiceConfig at startup based on the user's
	// config.json + auto-detection. Off means the row is rendered with
	// only the existing chevron (the legacy look) — important for
	// terminals or fonts that can't render the private-use glyphs.
	IconsEnabled bool
}

// New creates a tree rooted at root and pre-loads its top-level children so
// the user sees something immediately. Hidden entries (dotfiles) are kept
// because they're often what people actually want to inspect over SSH.
func New(root string) (*Tree, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, os.ErrInvalid
	}
	// Default ON, matching VS Code, which hides files.exclude entries out of the box. The tree
	// was the only view that did not already respect .gitignore — the fuzzy finder always has.
	ign := newIgnoreSet(abs)
	n := &Node{Path: abs, Name: filepath.Base(abs), IsDir: true, Expanded: true, ign: ign}
	if err := loadChildren(n); err != nil {
		return nil, err
	}
	return &Tree{Root: n, ignore: ign}, nil
}

// loadChildren is the lazy-load entry point used the first time a directory
// is expanded. It defers to reload, which knows how to merge fresh disk
// state with whatever (if anything) we already had cached.
func loadChildren(n *Node) error {
	if !n.IsDir || n.Loaded {
		return nil
	}
	return n.reload()
}

// reload re-reads the directory's children from disk and replaces n.Children
// with the new list. Existing child Nodes whose names still appear on disk
// are kept by-pointer so their Expanded state, loaded grandchildren, etc.
// survive a refresh. New names get fresh Nodes; vanished names are dropped.
func (n *Node) reload() error {
	if !n.IsDir {
		return nil
	}
	entries, err := os.ReadDir(n.Path)
	if err != nil {
		return err
	}

	existing := make(map[string]*Node, len(n.Children))
	for _, c := range n.Children {
		existing[c.Name] = c
	}

	children := make([]*Node, 0, len(entries))
	for _, e := range entries {
		if shouldHide(e.Name()) {
			continue
		}
		full := filepath.Join(n.Path, e.Name())
		if !n.ign.allows(full) {
			continue
		}
		if old, ok := existing[e.Name()]; ok && old.IsDir == e.IsDir() {
			old.ign = n.ign // inherit, in case the filter was toggled since this node was built
			children = append(children, old)
			continue
		}
		children = append(children, &Node{
			Path:  full,
			Name:  e.Name(),
			IsDir: e.IsDir(),
			ign:   n.ign,
		})
	}
	sort.SliceStable(children, func(i, j int) bool {
		if children[i].IsDir != children[j].IsDir {
			return children[i].IsDir
		}
		return strings.ToLower(children[i].Name) < strings.ToLower(children[j].Name)
	})
	n.Children = children
	n.Loaded = true
	return nil
}

// Refresh re-reads every directory in the tree that has been loaded at
// least once (i.e. anywhere the user has previously expanded). Surviving
// entries keep their Node pointers so deeper Expanded state is preserved;
// new files appear, deleted files vanish.
func (t *Tree) Refresh() {
	// Rebuild the ignore set first: .gitignore is itself an editable file, and a stale set would
	// keep showing a directory the user just ignored (or keep hiding one they just un-ignored)
	// until the editor restarted.
	if t.ignore != nil && t.ignore.active {
		fresh := newIgnoreSet(t.Root.Path)
		if fresh.active {
			*t.ignore = *fresh // in place, so every Node sharing the pointer sees it
		}
	}
	refreshNode(t.Root)
}

// refreshNode is Tree.Refresh's recursive worker. It reloads only Loaded
// directories — there's no value in reading directories the user has
// never seen.
func refreshNode(n *Node) {
	if !n.IsDir || !n.Loaded {
		return
	}
	_ = n.reload()
	for _, c := range n.Children {
		refreshNode(c)
	}
}

// shouldHide is the project's small, opinionated list of names the file
// tree refuses to show. These are universally noise: VCS metadata, OS
// junk, language-specific build caches.
func shouldHide(name string) bool {
	switch name {
	case ".git", ".svn", ".hg",
		".DS_Store",
		"node_modules",
		".idea", ".vscode":
		return true
	}
	return false
}

// flatNode pairs a Node with its render depth so the renderer can indent
// without re-walking the tree.
type flatNode struct {
	Node  *Node
	Depth int
}

// flattenInto appends node into out. If node is an expanded directory, it
// recursively appends its children at depth+1.
func flattenInto(n *Node, depth int, out *[]flatNode) {
	if n == nil {
		return
	}
	*out = append(*out, flatNode{Node: n, Depth: depth})
	if n.IsDir && n.Expanded {
		for _, c := range n.Children {
			flattenInto(c, depth+1, out)
		}
	}
}

// Render draws the tree into the rectangle (x, y, w, h). Each visible row
// is also remembered (in t.visible) so HitTest can map a click back to a
// node without re-walking the tree.
func (t *Tree) Render(scr tcell.Screen, th theme.Theme, x, y, w, h int) {
	bg := th.SidebarBG
	bgStyle := tcell.StyleDefault.Background(bg).Foreground(th.Text)
	for cy := y; cy < y+h; cy++ {
		for cx := x; cx < x+w; cx++ {
			scr.SetContent(cx, cy, ' ', nil, bgStyle)
		}
	}

	// Header — small all-caps label above the project name. The
	// project name itself is also a click target: it's the only way
	// to reset the active folder back to the root once a subfolder
	// has been selected. Render bold/Accent when it *is* the active
	// folder, plain text otherwise — same visual rule the children
	// rows follow, so the highlight is honest.
	headerStyle := tcell.StyleDefault.Background(bg).Foreground(th.Muted).Bold(true)
	drawString(scr, x, y, w, " EXPLORER", headerStyle)
	rootActive := t.ActiveFolder == "" || t.ActiveFolder == t.Root.Path
	rootStyle := tcell.StyleDefault.Background(bg).Foreground(th.Text).Bold(true)
	if rootActive {
		rootStyle = tcell.StyleDefault.Background(bg).Foreground(th.Accent).Bold(true)
	}
	if rootChange := t.DirtyFolders[t.Root.Path]; rootChange != GitChangeNone {
		rootStyle = rootStyle.Foreground(gitChangeColor(th, rootChange))
	}
	drawString(scr, x, y+1, w, " "+t.Root.Name, rootStyle)

	// Build the flat list of visible rows from the root's children.
	flat := make([]flatNode, 0, 128)
	for _, c := range t.Root.Children {
		flattenInto(c, 0, &flat)
	}

	listTop := y + 2
	listH := h - 2
	if listH < 0 {
		listH = 0
	}
	t.clampScroll(len(flat), listH)

	visible := make([]*Node, 0, listH)
	for row := 0; row < listH; row++ {
		idx := t.ScrollY + row
		if idx < 0 || idx >= len(flat) {
			visible = append(visible, nil)
			continue
		}
		item := flat[idx]
		active := item.Node.Path == t.ActiveFile || (item.Node.IsDir && item.Node.Path == t.ActiveFolder)
		change := t.changeKind(item.Node)
		drawNodeRow(scr, th, x, listTop+row, w, item, active, change, t.IconsEnabled)
		visible = append(visible, item.Node)
	}
	t.visible = visible
}

// changeKind returns the git status color category for a tree node.
func (t *Tree) changeKind(n *Node) GitChangeKind {
	if n == nil {
		return GitChangeNone
	}
	if n.IsDir {
		return t.DirtyFolders[n.Path]
	}
	return t.DirtyFiles[n.Path]
}

// drawNodeRow renders one tree row with proper indent, chevron, and color.
// active=true marks the active file or current working folder. change marks
// uncommitted git status and overrides the normal foreground so changed names
// stand out in the tree like other modern editors.
// withIcons=true prefixes the name with a Nerd Font glyph + space; off
// renders the legacy chevron-only look for terminals that can't show
// the private-use glyphs.
//
// When icons are enabled the row is rendered in three segments
// (prefix → glyph → name) so the glyph can take its own per-language
// colour while the name keeps the row's normal fg/dirty/active
// styling. That's the visual cue you find in nvim-tree and friends:
// a quick eye-scan picks out Go from Ruby from Markdown without
// reading any text.
// rowParts builds the two text chunks of a tree row: the left chunk (leading space, indent,
// chevron) and the right chunk (name, with a trailing slash for directories).
//
// Shared by drawNodeRow and NaturalWidth on purpose. The auto-fit width has to agree with what is
// actually painted to the column, and the only way to guarantee that is for both to build the same
// strings — a second copy of the indent-and-chevron arithmetic would drift the first time a glyph
// or a space changed.
func rowParts(item flatNode) (prefix, suffix string) {
	indent := strings.Repeat("  ", item.Depth)
	if item.Node.IsDir {
		chev := "▸"
		if item.Node.Expanded {
			chev = "▾"
		}
		return " " + indent + chev + " ", item.Node.Name + "/"
	}
	return " " + indent + "  ", item.Node.Name
}

// NaturalWidth reports how many columns the tree needs to draw every currently-expanded row, and
// both header rows, without clipping any name. Callers clamp it — this is the content requirement,
// not a recommendation, and a deep tree with long names can ask for more than the pane has.
//
// Counts only EXPANDED rows, which is what makes the result stable: it is independent of scroll
// position, so scrolling never changes the width, and it only moves when you expand or collapse a
// folder or the tree refreshes. Fitting the currently-scrolled-into-view rows instead would make
// the sidebar twitch as you scrolled, which is far worse than a clipped name.
func (t *Tree) NaturalWidth() int { return t.FitWidth(100) }

// FitWidth reports the width that accommodates the given PERCENTILE of currently-expanded rows,
// never dropping below what the two header rows need. FitWidth(100) is the widest row; NaturalWidth
// is exactly that.
//
// A percentile rather than the maximum, because the maximum is the wrong number to size a sidebar
// by. Sizing to it let ONE long name in an expanded folder inflate the whole panel: with scripts/
// open, "backfill-report-canonical-names.ts" wanted 43 columns and pinned the sidebar to 38% of a
// 114-column pane, which is worse than the fixed 30 it replaced and made resizing look inert
// because the panel just sat against its own ceiling. Clipping a rare long filename is what VS Code
// does anyway; clipping the folder names you navigate by is not.
func (t *Tree) FitWidth(percentile int) int {
	if t == nil || t.Root == nil {
		return 0
	}

	// The two header rows: " EXPLORER" and the project name. Always a floor -- a truncated project
	// name in the header is disorienting in a way a truncated row is not.
	need := len([]rune(" EXPLORER"))
	if n := len([]rune(" " + t.Root.Name)); n > need {
		need = n
	}

	flat := make([]flatNode, 0, 128)
	for _, c := range t.Root.Children {
		flattenInto(c, 0, &flat)
	}
	widths := make([]int, 0, len(flat))
	for _, item := range flat {
		prefix, suffix := rowParts(item)
		w := len([]rune(prefix)) + len([]rune(suffix))
		if t.IconsEnabled {
			// drawNodeRow paints prefix, then the glyph, then two spaces and the name.
			glyph := icons.For(item.Node.Name, item.Node.IsDir, item.Node.Expanded)
			w += len([]rune(glyph)) + 2
		}
		widths = append(widths, w)
	}
	if len(widths) == 0 {
		return need
	}

	if percentile < 1 {
		percentile = 1
	}
	if percentile > 100 {
		percentile = 100
	}
	sort.Ints(widths)
	// Ceiling index so FitWidth(100) is the last element and every percentile covers at least one
	// row. len=20, pct=85 -> idx 16, i.e. the 17th narrowest, clipping the three widest.
	idx := (len(widths)*percentile + 99) / 100
	if idx > len(widths) {
		idx = len(widths)
	}
	if w := widths[idx-1]; w > need {
		need = w
	}
	return need
}

func drawNodeRow(scr tcell.Screen, th theme.Theme, x, y, w int, item flatNode, active bool, change GitChangeKind, withIcons bool) {
	bg := th.SidebarBG

	// Compute the row-level foreground via this priority cascade
	// (highest wins last):
	//
	//   1. base = FolderColor / FileColor for the node type
	//   2. dotfile/dotdir → Muted, so .gitignore / .github read as
	//      "metadata, not source" without disappearing
	//   3. active folder → Accent, so the current target is loud
	//   4. dirty → Modified, so uncommitted work always stands out
	//
	// Active/dirty deliberately override the dotfile dimming — a
	// modified .env or the active .github/ folder is still the most
	// important thing on the row.
	var fg tcell.Color
	if item.Node.IsDir {
		fg = th.FolderColor
	} else {
		fg = th.FileColor
	}
	if strings.HasPrefix(item.Node.Name, ".") {
		fg = th.Muted
	}
	if active {
		fg = th.Accent
	}
	if change != GitChangeNone {
		fg = gitChangeColor(th, change)
	}
	rowStyle := tcell.StyleDefault.Background(bg).Foreground(fg)
	if active {
		rowStyle = rowStyle.Bold(true)
	}

	prefix, suffix := rowParts(item)

	if !withIcons {
		drawString(scr, x, y, w, prefix+suffix, rowStyle)
		return
	}

	glyph := icons.For(item.Node.Name, item.Node.IsDir, item.Node.Expanded)
	glyphFg := icons.ColorFor(item.Node.Name, item.Node.IsDir, fg)
	// Dirty files keep their per-language glyph colour — the language
	// hue is the at-a-glance cue, and the name turning Modified is
	// already enough to flag "this is dirty".
	glyphStyle := tcell.StyleDefault.Background(bg).Foreground(glyphFg)
	if active {
		glyphStyle = glyphStyle.Bold(true)
	}

	drawString(scr, x, y, w, prefix, rowStyle)
	px := len([]rune(prefix))
	drawString(scr, x+px, y, w-px, glyph, glyphStyle)
	gx := len([]rune(glyph))
	drawString(scr, x+px+gx, y, w-px-gx, "  "+suffix, rowStyle)
}

// gitChangeColor maps git status kinds to the tree row foreground.
func gitChangeColor(th theme.Theme, change GitChangeKind) tcell.Color {
	switch change {
	case GitChangeAdded:
		return th.GitAdded
	case GitChangeDeleted:
		return th.GitDeleted
	case GitChangeRenamed:
		return th.GitRenamed
	case GitChangeMixed:
		return th.GitMixed
	case GitChangeConflict:
		// Error, not GitModified: an unresolved conflict is a thing that BLOCKS
		// work, and it should not read as an ordinary edit at a glance.
		return th.Error
	case GitChangeModified:
		return th.GitModified
	}
	return th.FileColor
}

// drawString writes s left-aligned within [x, x+w). Excess content is
// truncated; short content is implicitly padded by the row's pre-painted bg.
func drawString(scr tcell.Screen, x, y, w int, s string, st tcell.Style) {
	col := 0
	for _, r := range s {
		if col >= w {
			return
		}
		scr.SetContent(x+col, y, r, nil, st)
		col++
	}
}

// clampScroll keeps ScrollY within bounds for the current visible-row count.
func (t *Tree) clampScroll(total, viewH int) {
	if total <= viewH {
		t.ScrollY = 0
		return
	}
	max := total - viewH
	if t.ScrollY > max {
		t.ScrollY = max
	}
	if t.ScrollY < 0 {
		t.ScrollY = 0
	}
}

// HitTest maps a click within the tree's render rectangle to a Node.
// Row 0 is the "EXPLORER" header (not clickable). Row 1 is the project
// root name — clicking it returns t.Root so the caller can set the
// active folder back to the project root, which is otherwise
// unreachable once the user has selected any subfolder. Rows 2+ map
// into the rendered children list.
//
// ok=false means the click landed on the EXPLORER header or empty
// space below the last entry.
func (t *Tree) HitTest(localX, localY int) (*Node, bool) {
	_ = localX
	if localY < 1 {
		return nil, false
	}
	if localY == 1 {
		return t.Root, true
	}
	row := localY - 2
	if row < 0 || row >= len(t.visible) {
		return nil, false
	}
	n := t.visible[row]
	if n == nil {
		return nil, false
	}
	return n, true
}

// Toggle expands or collapses a directory node, lazily loading its children
// the first time it is expanded.
func (t *Tree) Toggle(n *Node) {
	if !n.IsDir {
		return
	}
	if !n.Expanded {
		_ = loadChildren(n)
	}
	n.Expanded = !n.Expanded
}

// Scroll moves the file tree's viewport by delta rows (negative = up).
func (t *Tree) Scroll(delta int) {
	t.ScrollY += delta
	if t.ScrollY < 0 {
		t.ScrollY = 0
	}
}

// Reveal expands every directory from the tree root down to path's parent so
// the file becomes visible in the sidebar, then scrolls the viewport so the
// row lands on screen. Opening a file via the finder (Esc-p) or the command
// line lands on a path whose ancestors are still collapsed — without this,
// the active-file highlight is set but the row itself is invisible, leaving
// the sidebar out of sync with the editor like a tab with no tab bar entry.
//
// When the target row is already inside the current viewport the scroll
// position is left untouched, so clicking a visible row in the tree (which
// also routes through openFile) doesn't snap it to the top.
//
// No-op when path isn't under the root, escapes it, or lives inside a hidden
// directory the tree refuses to show (e.g. .git). viewH is the row count the
// renderer will hand Render's list area; pass 0 to expand ancestors without
// scrolling (used when the sidebar is hidden).
func (t *Tree) Reveal(path string, viewH int) {
	if t.Root == nil {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	rel, err := filepath.Rel(t.Root.Path, abs)
	if err != nil {
		return
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")

	// Walk every directory component, lazily loading + expanding each so the
	// next step can descend into it. The final component is the target row
	// itself; it doesn't need expanding — revealing is about visibility, not
	// auto-opening directories.
	n := t.Root
	for i := 0; i < len(parts)-1; i++ {
		if !n.Loaded {
			_ = loadChildren(n)
		}
		child := childByName(n, parts[i])
		if child == nil {
			return // hidden or gone — can't descend further
		}
		if !child.Expanded {
			child.Expanded = true
			if !child.Loaded {
				_ = loadChildren(child)
			}
		}
		n = child
	}

	// Find the target row among its parent's children so we can scroll to it.
	if !n.Loaded {
		_ = loadChildren(n)
	}
	target := childByName(n, parts[len(parts)-1])
	if target == nil {
		return
	}

	idx := t.flatIndexOf(target)
	if idx < 0 {
		return
	}
	if viewH <= 0 {
		return
	}
	// Leave the viewport alone when the row is already on screen — a click on
	// a visible row shouldn't snap it to the top.
	if idx >= t.ScrollY && idx < t.ScrollY+viewH {
		return
	}
	t.ScrollY = idx
}

// flatIndexOf returns the row index of target in the renderer's flat list
// (the same pre-order walk Render builds via flattenInto), or -1 when target
// isn't currently visible. Mirrors the render order exactly so the index we
// scroll to is the row the user actually sees.
func (t *Tree) flatIndexOf(target *Node) int {
	idx := 0
	var walk func(n *Node) bool
	walk = func(n *Node) bool {
		if n == target {
			return true
		}
		idx++
		if n.IsDir && n.Expanded {
			for _, c := range n.Children {
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	for _, c := range t.Root.Children {
		if walk(c) {
			return idx
		}
	}
	return -1
}

// childByName returns the direct child of n named name, or nil when no such
// child exists. Reveal uses it to descend the path component by component.
func childByName(n *Node, name string) *Node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}
