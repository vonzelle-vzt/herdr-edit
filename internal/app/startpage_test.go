// =============================================================================
// File: internal/app/startpage_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/filetree"
)

// screenAll returns the whole simulated screen as one string, so a test can assert on what the
// user would actually read rather than on layout arithmetic.
func screenAll(scr tcell.SimulationScreen) string {
	cells, w, h := scr.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := cells[y*w+x].Runes
			if len(r) == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(r[0])
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// paint draws the app at the given size and hands back what landed on screen.
func paint(t *testing.T, a *App, w, h int) string {
	t.Helper()
	scr := a.screen.(tcell.SimulationScreen)
	scr.SetSize(w, h)
	a.width, a.height = w, h
	a.draw()
	scr.Show() // SimulationScreen serves GetContents from the front buffer
	return screenAll(scr)
}

// TestStartPageListsChangedFiles pins down the replacement for the old "No file open" placeholder:
// with no tab open the pane must show where you are and what has changed, because that pane is
// often the widest thing on screen and two lines of grey text wasted all of it.
func TestStartPageListsChangedFiles(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"alpha.go", "beta.ts"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := newTestApp(t, root)
	a.gitBranch = "main"
	a.tree.DirtyFiles = map[string]filetree.GitChangeKind{
		filepath.Join(root, "alpha.go"): filetree.GitChangeModified,
		filepath.Join(root, "beta.ts"):  filetree.GitChangeAdded,
	}

	out := paint(t, a, 120, 40)
	for _, want := range []string{"2 changed files", "alpha.go", "beta.ts", "on main", "Esc p"} {
		if !strings.Contains(out, want) {
			t.Errorf("start page missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "No file open") {
		t.Error("the old placeholder is still being drawn")
	}
}

// TestStartPageWithoutRepo keeps the no-git case honest: it must say so plainly rather than
// rendering an empty "changed files" heading.
func TestStartPageWithoutRepo(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	out := paint(t, a, 120, 40)
	if !strings.Contains(out, "Not a git repository") {
		t.Errorf("expected the non-repo hint\n%s", out)
	}
}

// TestStartPageClickOpensFile is the reason startRows exists: the changed-file list is only useful
// if the entries are actionable.
func TestStartPageClickOpensFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "alpha.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, root)
	a.tree.DirtyFiles = map[string]filetree.GitChangeKind{target: filetree.GitChangeModified}
	paint(t, a, 120, 40)

	if len(a.startRows) != 1 {
		t.Fatalf("expected one clickable row, got %d", len(a.startRows))
	}
	row := a.startRows[0]
	if !a.handleStartPageClick(row.y, row.y) && a.activeTabPtr() == nil {
		// x is deliberately inside the editor rect; use the row's own y for both to stay simple.
	}
	ex, _, _, _ := a.editorRect()
	if !a.handleStartPageClick(ex+3, row.y) {
		t.Fatal("click on a changed-file row was not consumed")
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.Path != target {
		t.Fatalf("click did not open the file, active tab = %+v", tab)
	}
}

// TestStartPageClickIgnoredWhenATabIsOpen guards the fall-through: once a file is open the editor
// owns those cells and a click must place the cursor, not re-open something.
func TestStartPageClickIgnoredWhenATabIsOpen(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, root)
	a.openFile(target)
	if a.handleStartPageClick(10, 5) {
		t.Fatal("start page consumed a click while a tab was open")
	}
}

// TestNarrowPaneNarrowsTreeInsteadOfRefusingToDraw is the whole point of the responsive work.
// Previously anything under 50 columns replaced the entire editor with "Window too small", and
// since the file tree alone is a fixed 30 columns a side panel was only usable in a narrow band.
//
// The tree now NARROWS on a tight pane rather than hiding: a 52-column pane keeps a readable tree
// AND draws the editor. Hiding outright below 76 columns switched the explorer off in exactly the
// place it earns its keep — a herdr split beside an agent, where the pane is 60-odd columns.
func TestNarrowPaneNarrowsTreeInsteadOfRefusingToDraw(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, root)
	a.openFile(filepath.Join(root, "main.go"))

	// Wide: the tree earns its full preferred width.
	if out := paint(t, a, 120, 40); !a.sidebarVisible() {
		t.Errorf("tree should be visible at 120 cols\n%s", out)
	}
	if got := a.sidebarW(); got != defaultSidebarWidth {
		t.Errorf("120 cols should give the tree its full width: got %d, want %d",
			got, defaultSidebarWidth)
	}

	// Narrow: the tree gives up columns but stays on screen, and the editor keeps drawing.
	out := paint(t, a, 52, 30)
	if !a.sidebarVisible() {
		t.Errorf("tree should narrow, not hide, at 52 cols\n%s", out)
	}
	if got := a.sidebarW(); got < minSidebarWidth || got >= defaultSidebarWidth {
		t.Errorf("52 cols should narrow the tree into [%d, %d): got %d",
			minSidebarWidth, defaultSidebarWidth, got)
	}
	if strings.Contains(out, "Window too small") {
		t.Errorf("52 columns must still render the editor\n%s", out)
	}
	if !strings.Contains(out, "package main") {
		t.Errorf("editor content missing at 52 cols\n%s", out)
	}

	// Below treeNeeds it does finally stand down — the floor still exists.
	out = paint(t, a, treeNeeds-1, 30)
	if a.sidebarVisible() {
		t.Errorf("tree should hide below treeNeeds (%d)\n%s", treeNeeds, out)
	}
	if strings.Contains(out, "Window too small") {
		t.Errorf("%d columns must still render the editor\n%s", treeNeeds-1, out)
	}

	// The preference survives, so widening brings it straight back with no keypress.
	if !a.sidebarShown {
		t.Error("auto-hide must not clear the user preference")
	}
	if !paintHasTree(t, a) {
		t.Error("tree did not come back when the pane widened again")
	}
}

func paintHasTree(t *testing.T, a *App) bool {
	t.Helper()
	paint(t, a, 120, 40)
	return a.sidebarVisible()
}

// TestTooSmallOnlyAtGenuinelyUnusableSizes checks we did not simply delete the guard: there is
// still a floor, it is just far below where a side panel lives.
func TestTooSmallOnlyAtGenuinelyUnusableSizes(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if out := paint(t, a, minWidth-1, 30); !strings.Contains(out, "Window too small") {
		t.Errorf("expected the too-small notice below minWidth\n%s", out)
	}
	if out := paint(t, a, 40, minHeight-1); !strings.Contains(out, "Window too small") {
		t.Errorf("expected the too-small notice below minHeight\n%s", out)
	}
	if out := paint(t, a, minWidth, minHeight); strings.Contains(out, "Window too small") {
		t.Errorf("minWidth x minHeight should be drawable\n%s", out)
	}
}

// TestSidebarToggleExplainsAutoHide covers the confusing case: with the tree already auto-hidden,
// flipping the preference would look like the toggle did nothing at all.
func TestSidebarToggleExplainsAutoHide(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.width = treeNeeds - 1
	a.menuToggleSidebar()
	if !a.sidebarShown {
		t.Error("toggle must not flip the preference while the tree is auto-hidden")
	}
	if !strings.Contains(a.statusMsg, "hidden automatically") {
		t.Errorf("expected an explanation, got %q", a.statusMsg)
	}
	if got := a.sidebarToggleLabel(); !strings.Contains(got, "too narrow") {
		t.Errorf("menu label should describe reality, got %q", got)
	}
}

// TestRelativeToRoot_ResolvesSymlinks pins the bug a real render exposed. On
// macOS /tmp IS /private/tmp, so a project opened by one name against a git
// status reporting the other produced "../../../../private/tmp/..." — correct,
// unreadable, and it pushes the filename off the end of the line.
func TestRelativeToRoot_ResolvesSymlinks(t *testing.T) {
	dir := t.TempDir() // under /var, itself a symlink to /private/var on macOS
	sub := filepath.Join(dir, "src", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "routes.go")
	if err := os.WriteFile(file, []byte("package api\n"), 0644); err != nil {
		t.Fatal(err)
	}

	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	// The root is the UNRESOLVED name, the file the RESOLVED one — exactly the
	// mismatch git status produces.
	got := relativeToRoot(dir, resolved)
	if want := filepath.Join("src", "api", "routes.go"); got != want {
		t.Fatalf("relativeToRoot = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, "..") {
		t.Fatal("the path escaped the root as a chain of dots")
	}
}

// TestRelativeToRoot_OutsideRoot pins that a genuinely unrelated path shows as
// absolute rather than as a ladder of dots.
func TestRelativeToRoot_OutsideRoot(t *testing.T) {
	got := relativeToRoot(t.TempDir(), "/etc/hosts")
	if got != "/etc/hosts" {
		t.Fatalf("relativeToRoot = %q, want the absolute path", got)
	}
}
