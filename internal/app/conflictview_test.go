// =============================================================================
// File: internal/app/conflictview_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Tests for conflictview.go — and for the two claims the whole conflict
// feature rests on, both of which are about the SEAM between git and the
// buffer rather than about either one alone:
//
//   - the glyph and the tint land on the rows the markers are actually drawn
//     on, in both geometries, and
//   - a file whose bytes are a perfect conflict shows NOTHING when git says
//     the file is clean.
//
// Every screen assertion here is derived from a rendered SimulationScreen —
// scanned for where a glyph or a colour actually landed — never from a second
// copy of the geometry the code under test uses. CLAUDE.md's ScreenPos note
// records what happens otherwise: the same wrong formula on both sides of the
// assertion, twice, shipped.
package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// conflictGutterGlyph is the rune the gutter is expected to carry on a marker
// line. Spelled out here rather than imported: editor's constant is
// unexported, and a test that reads the value it is checking from the code
// under test asserts nothing about what a user sees.
const conflictGutterGlyph = '▚'

// markerRunRe matches a run of at least seven identical conflict marker runes
// — the same shape git writes, used here to find marker ROWS on the rendered
// screen without knowing anything about the scanner's internals.
var markerRunRe = regexp.MustCompile(`<{7,}|\|{7,}|={7,}|>{7,}`)

// screenRows renders the app and returns one string per screen row, so an
// assertion can talk about what is on screen instead of about coordinates.
func screenRows(t *testing.T, a *App) []string {
	t.Helper()
	scr := a.screen.(tcell.SimulationScreen)
	a.draw()
	scr.Show()
	cells, w, h := scr.GetContents()
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			if r := cells[y*w+x].Runes; len(r) > 0 {
				b.WriteRune(r[0])
			} else {
				b.WriteRune(' ')
			}
		}
		rows[y] = b.String()
	}
	return rows
}

// cleanRepoContainingMarkers commits a file whose bytes are a complete,
// well-formed conflict and leaves the repo CLEAN. This is the file the editor
// must say nothing about — and the repo has one in real life the moment this
// package's own conflict_test.go is checked out.
func cleanRepoContainingMarkers(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; this oracle needs a real clean repo")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	file := filepath.Join(root, "fixture.go")
	body := strings.Join([]string{
		"package fixture",
		"",
		"// A conflict fixture, committed and clean. These bytes are IDENTICAL to",
		"// a real conflict — that is the whole point.",
		"var sample = []string{",
		"<<<<<<< HEAD",
		"\t\"ours\",",
		"=======",
		"\t\"theirs\",",
		">>>>>>> feature",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	run("add", "fixture.go")
	run("commit", "-qm", "a file full of markers, and nothing is wrong")
	return root, file
}

// assertGlyphIsOnMarkerRowsOnly renders and requires the set of rows carrying
// the conflict glyph to be EXACTLY the set of rows showing marker text.
//
// 🔴 Both sides are scanned off the rendered screen. Nothing here computes
// "the marker is on buffer line N so it should be at row N + editorY", which
// is the assertion shape that let two separate off-by-a-gutter bugs ship in
// this repo: it restates the arithmetic under test and therefore measures
// nothing. It also means the same assertion works unchanged for a wrapped tab,
// where that arithmetic is simply not true.
func assertGlyphIsOnMarkerRowsOnly(t *testing.T, a *App, what string) {
	t.Helper()
	rows := screenRows(t, a)

	glyphAt := map[int]int{} // row -> column
	markerRows := map[int]bool{}
	for y, row := range rows {
		if x := strings.IndexRune(row, conflictGutterGlyph); x >= 0 {
			glyphAt[y] = x
			if strings.Count(row, string(conflictGutterGlyph)) != 1 {
				t.Errorf("%s: row %d carries the conflict glyph more than once: %q", what, y, row)
			}
		}
		if markerRunRe.MatchString(row) {
			markerRows[y] = true
		}
	}

	if len(markerRows) == 0 {
		t.Fatalf("%s: no conflict marker text is on screen at all — the fixture never rendered", what)
	}
	for y := range markerRows {
		if _, ok := glyphAt[y]; !ok {
			t.Errorf("%s: row %d shows a marker but has no gutter glyph: %q", what, y, rows[y])
		}
	}
	for y := range glyphAt {
		if !markerRows[y] {
			t.Errorf("%s: row %d carries the conflict glyph but shows no marker: %q", what, y, rows[y])
		}
	}

	// One column for every glyph. A rune from an ambiguous-width block would
	// render two cells wide in some terminals and shift everything to its
	// right; a glyph that wandered between rows would mean the gutter width is
	// being recomputed somewhere it should not be.
	col := -1
	for y, x := range glyphAt {
		if col == -1 {
			col = x
			continue
		}
		if x != col {
			t.Errorf("%s: glyph column moved between rows (row %d at x=%d, another at x=%d)", what, y, x, col)
		}
	}
}

// TestConflictGlyphRendersOnMarkerLinesAndNowhereElse is the gutter oracle,
// run against a REAL git conflict in both of git's conflict styles — so diff3's
// extra ||||||| marker line has to be handled, not merely tolerated.
//
// The second half re-renders the same tab WRAPPED, with a line longer than the
// viewport above the conflict so the marker rows are genuinely no longer
// `line + editorY`. That path gets the glyph for free because gutterMarker is
// the single glyph-priority point BOTH render loops call; this is what proves
// the "for free" claim rather than asserting it in a comment.
func TestConflictGlyphRendersOnMarkerLinesAndNowhereElse(t *testing.T) {
	for _, style := range []string{"merge", "diff3"} {
		t.Run(style, func(t *testing.T) {
			root, file := makeConflictedRepo(t, style)
			a := newTestApp(t, root)
			a.openFile(file)

			tab := a.activeTabPtr()
			if tab == nil {
				t.Fatal("the conflicted file did not open")
			}
			if !tab.GitUnmerged {
				t.Fatal("git says the file is unmerged but the tab was never told")
			}
			if len(tab.Conflicts) != 1 {
				t.Fatalf("scanned %d regions in the fixture, want 1: %+v", len(tab.Conflicts), tab.Conflicts)
			}
			wantMarkers := 3
			if style == "diff3" {
				wantMarkers = 4
			}

			assertGlyphIsOnMarkerRowsOnly(t, a, "unwrapped/"+style)

			rows := screenRows(t, a)
			got := 0
			for _, row := range rows {
				if strings.ContainsRune(row, conflictGutterGlyph) {
					got++
				}
			}
			if got != wantMarkers {
				t.Errorf("%s style: %d glyph rows, want %d (one per marker line)", style, got, wantMarkers)
			}

			// Force real wrapping: a line far longer than the viewport, above
			// the conflict, so every marker row moves by the number of
			// continuation rows it introduces. Assigning Lines directly is
			// safe here only because this tab carries no marks.
			tab.Buffer.Lines = append([]string{strings.Repeat("x", 400)}, tab.Buffer.Lines...)
			tab.RescanConflicts()
			tab.Wrap = true

			wrapped := screenRows(t, a)
			if !strings.Contains(strings.Join(wrapped, "\n"), "↪") {
				t.Fatal("the fixture did not actually wrap, so the wrapped path was never exercised")
			}
			assertGlyphIsOnMarkerRowsOnly(t, a, "wrapped/"+style)
		})
	}
}

// TestConflictsStayEmptyWhenGitSaysTheFileIsClean is the entire defence
// against a false positive, and it cannot be replaced by any test on the
// buffer: the bytes of a marker inside a string literal, a fixture or a
// merge-tool README are IDENTICAL to a real one.
//
// So the file here is a complete, well-formed conflict sequence, committed
// clean. The editor must show nothing — and the second assertion proves that
// is git's verdict doing the work rather than a scanner that simply failed to
// parse the file.
func TestConflictsStayEmptyWhenGitSaysTheFileIsClean(t *testing.T) {
	root, file := cleanRepoContainingMarkers(t)
	a := newTestApp(t, root)
	a.openFile(file)

	tab := a.activeTabPtr()
	if tab == nil {
		t.Fatal("the fixture did not open")
	}
	if tab.GitUnmerged {
		t.Error("a clean file was marked unmerged")
	}
	if len(tab.Conflicts) != 0 {
		t.Fatalf("a CLEAN file scanned to %d conflict regions: %+v", len(tab.Conflicts), tab.Conflicts)
	}

	// The bytes really are a conflict — it is git's verdict that suppressed
	// them, not a scanner that could not read them.
	if n := len(editor.ScanConflicts(tab.Buffer.Lines)); n != 1 {
		t.Fatalf("the same bytes scan to %d regions directly, want 1 — the fixture is wrong, not the gate", n)
	}

	// Nothing on screen either: no gutter glyph, and no menu rows.
	for y, row := range screenRows(t, a) {
		if strings.ContainsRune(row, conflictGutterGlyph) {
			t.Errorf("row %d painted a conflict glyph on a clean file: %q", y, row)
		}
	}
	if a.hasConflicts() {
		t.Error("the conflict menu group would be visible on a clean file")
	}
}

// TestConflictTintMarksTheTwoSidesDifferently reads the BACKGROUND colour off
// the rendered screen. A tint is the only part of this feature with no text to
// check, so the pixel is the whole evidence — and getting it wrong (both sides
// the same colour) would look like a working feature right up until someone
// picked the wrong half of a merge.
func TestConflictTintMarksTheTwoSidesDifferently(t *testing.T) {
	root, file := makeConflictedRepo(t, "diff3")
	a := newTestApp(t, root)
	a.openFile(file)
	tab := a.activeTabPtr()
	if tab == nil || len(tab.Conflicts) != 1 {
		t.Fatalf("fixture did not produce exactly one region: %+v", tab)
	}
	c := tab.Conflicts[0]

	scr := a.screen.(tcell.SimulationScreen)
	a.draw()
	scr.Show()
	cells, w, _ := scr.GetContents()
	ex, ey, ew, eh := a.editorRect()

	// Read the background at the FIRST content cell of a line, positioned by
	// ScreenPos — the same contract every other overlay in this fork uses.
	// 🔴 Style.Decompose returns (fg, bg, attr) — foreground FIRST. Reading the
	// first value as the background is silently wrong rather than a type
	// error, and it is what this assertion caught on its first run.
	bgAt := func(line int) tcell.Color {
		t.Helper()
		dx, dy, ok := tab.ScreenPos(line, 0, ew, eh)
		if !ok {
			t.Fatalf("line %d is not visible; the fixture is too tall for the test screen", line)
		}
		_, bg, _ := cells[(ey+dy)*w+(ex+dx)].Style.Decompose()
		return bg
	}

	for _, tc := range []struct {
		name string
		line int
		want tcell.Color
	}{
		{"ours body", c.Start + 1, a.theme.ConflictOurs},
		{"base body", c.Base + 1, a.theme.ConflictBase},
		{"theirs body", c.Sep + 1, a.theme.ConflictTheirs},
		{"the ======= line itself", c.Sep, a.theme.ConflictTheirs},
	} {
		if got := bgAt(tc.line); got != tc.want {
			t.Errorf("%s (line %d): background %v, want %v", tc.name, tc.line, got, tc.want)
		}
	}

	// And nothing outside the region is tinted.
	if got := bgAt(0); got == a.theme.ConflictOurs || got == a.theme.ConflictTheirs || got == a.theme.ConflictBase {
		t.Errorf("line 0 is outside every region but was tinted %v", got)
	}
}

// TestConflictTintIsSkippedOnAWrappedTab pins the deliberate cut. The GLYPH
// works wrapped because gutterMarker is shared; the TINT does not, because
// Tab.ScreenPos assumes one buffer line is one screen row. A tint painted on
// the wrong rows is worse than none — it would claim lines belong to a side
// they do not — so the overlay bails out instead of guessing.
func TestConflictTintIsSkippedOnAWrappedTab(t *testing.T) {
	root, file := makeConflictedRepo(t, "merge")
	a := newTestApp(t, root)
	a.openFile(file)
	tab := a.activeTabPtr()
	tab.Wrap = true

	scr := a.screen.(tcell.SimulationScreen)
	a.draw()
	scr.Show()
	cells, w, h := scr.GetContents()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			_, bg, _ := cells[y*w+x].Style.Decompose() // (fg, bg, attr)
			if bg == a.theme.ConflictOurs || bg == a.theme.ConflictTheirs || bg == a.theme.ConflictBase {
				t.Fatalf("a wrapped tab was tinted at (%d,%d)", x, y)
			}
		}
	}
	// The glyph, however, must still be there — that is the whole reason the
	// two are handled by different mechanisms.
	found := false
	for _, row := range screenRows(t, a) {
		if strings.ContainsRune(row, conflictGutterGlyph) {
			found = true
		}
	}
	if !found {
		t.Error("a wrapped tab lost the conflict gutter glyph as well as the tint")
	}
}

// conflictMenuLabels is the list every reachability assertion below checks
// against — written out once, so the test and the menu cannot drift apart
// silently in two places.
// conflictMenuLabels DERIVES the labels from conflictMenuGroup rather than
// restating them.
//
// 🔴 It used to be a hand-written copy, and that copy is what an oracle cannot
// afford: renaming the actions so the command palette could actually find them
// (they lacked the word "conflict", so the one term a user would type returned
// only the two navigation rows) turned this test red for a rename, while a
// genuinely MISSING action would look identical. A restated list polices
// nothing and cries wolf — the same failure as the panel-title list in the
// sibling repo.
func conflictMenuLabels() []string {
	var out []string
	for _, it := range conflictMenuGroup() {
		out = append(out, it.label)
	}
	return out
}

// TestConflictActionsAreReachableFromTheMenuAndThePalette is the
// has-this-shipped-unreachable guard.
//
// 🔴 CLAUDE.md records this exact failure twice: Tab.Replace / ReplaceAll and
// two LSP requests were complete, unit-tested and advertised as shipped
// features whose only callers were _test.go files. A green model test proves
// the engine works, not that anyone can reach it — so this one goes through
// the real menu, through the real palette, and then actually FIRES the action
// and checks the buffer changed.
//
// The clean-file half matters just as much: the rows use `visible`, not
// `enabled`, so a file with no conflict must see the menu it has always seen
// rather than seven greyed-out rows pushing Quit off the bottom.
func TestConflictActionsAreReachableFromTheMenuAndThePalette(t *testing.T) {
	root, file := makeConflictedRepo(t, "merge")
	a := newTestApp(t, root)
	a.openFile(file)
	tab := a.activeTabPtr()
	if tab == nil || len(tab.Conflicts) != 1 {
		t.Fatalf("fixture did not produce exactly one region")
	}

	menuLabels := map[string]bool{}
	items, _, _ := a.menuLayout()
	for _, it := range items {
		label := it.label
		if it.labelFor != nil {
			label = it.labelFor(a)
		}
		menuLabels[label] = true
	}
	paletteLabels := map[string]bool{}
	for _, cmd := range a.paletteCommands() {
		paletteLabels[cmd.label] = true
	}
	for _, want := range conflictMenuLabels() {
		if !menuLabels[want] {
			t.Errorf("action %q is not in the ≡ menu", want)
		}
		if !paletteLabels[want] {
			t.Errorf("action %q is not in the command palette", want)
		}
	}

	// Fire it for real, through the palette entry a user would click.
	tab.MoveCursorTo(editor.Position{Line: tab.Conflicts[0].Start + 1, Col: 0}, false)
	if !a.hasConflictAtCursor() {
		t.Fatal("the cursor is inside the region but the enable predicate disagrees")
	}
	fired := false
	for _, cmd := range a.paletteCommands() {
		if cmd.label != conflictMenuGroup()[0].label {
			continue
		}
		if !cmd.enabled(a) {
			t.Fatal("Take ours is disabled with the cursor inside a conflict")
		}
		cmd.action(a)
		fired = true
	}
	if !fired {
		t.Fatal("no palette command matched — the action is unreachable")
	}
	if body := tab.Buffer.String(); strings.Contains(body, "<<<<<<<") || strings.Contains(body, "THEIRS") {
		t.Errorf("the palette action did not resolve the conflict:\n%s", body)
	}
	if !tab.Dirty {
		t.Error("resolving a conflict left the tab clean")
	}
	if len(tab.Conflicts) != 0 {
		t.Errorf("%d regions still cached after resolving the only one", len(tab.Conflicts))
	}

	// 🔴 No new leader rune was taken. Every lowercase letter is bound or
	// reserved; c / x / v belong to the terminal's own clipboard.
	if leaderActionFor('c') != nil {
		t.Error("something bound Esc c — it is RESERVED for the terminal's clipboard")
	}

	// A clean file's menu is unchanged.
	cleanRoot, cleanFile := cleanRepoContainingMarkers(t)
	b := newTestApp(t, cleanRoot)
	b.openFile(cleanFile)
	cleanItems, _, _ := b.menuLayout()
	for _, it := range cleanItems {
		for _, unwanted := range conflictMenuLabels() {
			if it.label == unwanted {
				t.Errorf("a clean file's menu still offers %q", unwanted)
			}
		}
	}
	if len(cleanItems) >= len(items) {
		t.Errorf("the conflict group did not add any rows on the conflicted file (%d vs %d)",
			len(items), len(cleanItems))
	}
}

// TestNextAndPreviousConflictWalkTheFileAndWrap covers the two navigation
// rows. Wrapping is what makes them usable on a file with one conflict at the
// top and another at the bottom — without it "Next conflict" would do nothing
// at all once you were past the last one, which reads as a broken row.
func TestNextAndPreviousConflictWalkTheFileAndWrap(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	tab := &editor.Tab{Path: "x.go", Buffer: editor.NewBuffer(strings.Join([]string{
		"top",
		"<<<<<<< HEAD", "\tone ours", "=======", "\tone theirs", ">>>>>>> b",
		"middle",
		"<<<<<<< HEAD", "\ttwo ours", "=======", "\ttwo theirs", ">>>>>>> b",
		"bottom",
	}, "\n"))}
	tab.GitUnmerged = true
	tab.RescanConflicts()
	a.tabs = append(a.tabs, tab)
	a.activeTab = 0
	if len(tab.Conflicts) != 2 {
		t.Fatalf("fixture has %d regions, want 2", len(tab.Conflicts))
	}

	a.menuConflictNext()
	if got := tab.Cursor.Line; got != 1 {
		t.Errorf("first Next landed on line %d, want 1", got)
	}
	a.menuConflictNext()
	if got := tab.Cursor.Line; got != 7 {
		t.Errorf("second Next landed on line %d, want 7", got)
	}
	a.menuConflictNext() // wraps
	if got := tab.Cursor.Line; got != 1 {
		t.Errorf("Next past the last conflict landed on line %d, want a wrap to 1", got)
	}
	a.menuConflictPrev() // wraps backwards
	if got := tab.Cursor.Line; got != 7 {
		t.Errorf("Prev before the first conflict landed on line %d, want a wrap to 7", got)
	}
	a.menuConflictPrev()
	if got := tab.Cursor.Line; got != 1 {
		t.Errorf("Prev landed on line %d, want 1", got)
	}
}

// TestResolveAllGoesThroughTheConfirmDialog pins the gate. Rewriting every
// conflict in a file with one click is exactly the class of action this fork
// already puts behind openConfirm, and the callback — not the menu row — is
// what does the work.
func TestResolveAllGoesThroughTheConfirmDialog(t *testing.T) {
	root, file := makeConflictedRepo(t, "merge")
	a := newTestApp(t, root)
	a.openFile(file)
	tab := a.activeTabPtr()
	before := tab.Buffer.String()

	a.menuConflictAllTheirs()
	if !a.confirmOpen {
		t.Fatal("Resolve all did not open a confirm dialog")
	}
	if got := tab.Buffer.String(); got != before {
		t.Fatal("Resolve all edited the buffer before the user confirmed")
	}
	if a.confirmCallback == nil {
		t.Fatal("the confirm has no callback, so answering Yes would do nothing")
	}
	a.confirmCallback(a)
	body := tab.Buffer.String()
	if strings.Contains(body, "<<<<<<<") {
		t.Errorf("confirming did not resolve the file:\n%s", body)
	}
	if !strings.Contains(body, "THEIRS") || strings.Contains(body, "OURS") {
		t.Errorf("Resolve all as theirs kept the wrong side:\n%s", body)
	}
}

// TestResolvingEverythingSaysWhatToDoNext pins the closing message. Writing
// the buffer clears the markers on disk, but only `git add` tells git the path
// is resolved — and the editor deliberately does not stage for the user, so
// the one place that gap can be closed is the status flash.
func TestResolvingEverythingSaysWhatToDoNext(t *testing.T) {
	root, file := makeConflictedRepo(t, "merge")
	a := newTestApp(t, root)
	a.openFile(file)
	tab := a.activeTabPtr()
	tab.MoveCursorTo(editor.Position{Line: tab.Conflicts[0].Start, Col: 0}, false)

	a.menuConflictTakeOurs()
	if !strings.Contains(a.statusMsg, "git add") {
		t.Errorf("after the last conflict the status said %q, which never mentions the "+
			"one step the editor does not take for you", a.statusMsg)
	}
}

// TestResolvingWithTheCursorOutsideAConflictSaysSo covers the no-op path. A
// silent nothing there reads as the menu row being broken, which is how a user
// concludes the feature does not work.
func TestResolvingWithTheCursorOutsideAConflictSaysSo(t *testing.T) {
	root, file := makeConflictedRepo(t, "merge")
	a := newTestApp(t, root)
	a.openFile(file)
	tab := a.activeTabPtr()
	tab.MoveCursorTo(editor.Position{Line: 0, Col: 0}, false)
	before := tab.Buffer.String()

	a.menuConflictTakeTheirs()
	if got := tab.Buffer.String(); got != before {
		t.Fatal("a resolution fired with the cursor outside every conflict")
	}
	if !strings.Contains(a.statusMsg, "cursor") {
		t.Errorf("status said %q, which does not tell the user what to do", a.statusMsg)
	}
}

// TestRefreshTabGitStateKeepsBothFactsInStep is the reason that function
// exists. GitLines and GitUnmerged were being kept in step by three separate
// call sites; two facts about the same tab maintained in three places is two
// facts that eventually disagree, and nothing on screen would say which was
// right.
func TestRefreshTabGitStateKeepsBothFactsInStep(t *testing.T) {
	root, file := makeConflictedRepo(t, "merge")
	a := newTestApp(t, root)

	tab, err := editor.NewTab(file)
	if err != nil {
		t.Fatal(err)
	}
	a.refreshTabGitState(tab)
	if !tab.GitUnmerged {
		t.Error("refreshTabGitState did not pick up git's unmerged verdict")
	}
	if len(tab.Conflicts) == 0 {
		t.Error("refreshTabGitState did not populate the region cache")
	}
	if len(tab.GitLines) == 0 {
		t.Error("refreshTabGitState dropped the line-change bars it used to set")
	}

	// Synthetic and image tabs have no path git has an opinion about, and must
	// not be scanned at all.
	syn := &editor.Tab{Synthetic: true, Label: "diff", Buffer: editor.NewBuffer("<<<<<<< a\n=======\n>>>>>>> b")}
	a.refreshTabGitState(syn)
	if syn.GitUnmerged || len(syn.Conflicts) != 0 {
		t.Error("a synthetic tab was given git state")
	}
}

// TestTintNeverPaintsOutsideTheEditorRect is the coordinate guard, in the same
// shape TestOverlaySkipsScrolledOffLines uses for diagnostics: with the whole
// region scrolled far above the viewport, not one cell anywhere on the screen
// may carry a conflict tint. Without the clip and the buffer-bounds check, a
// region the viewport has left behind is still walked line by line, and a
// stale one can name a line the buffer no longer has — which ScreenPos would
// happily place, because it bounds-checks against the VIEWPORT, not the
// buffer.
func TestTintNeverPaintsOutsideTheEditorRect(t *testing.T) {
	root, file := makeConflictedRepo(t, "diff3")
	a := newTestApp(t, root)
	a.openFile(file)
	tab := a.activeTabPtr()
	if len(tab.Conflicts) != 1 {
		t.Fatalf("fixture did not produce one region")
	}

	// Pad the file out so the conflict can be scrolled off the top, then park
	// the viewport well below it.
	for i := 0; i < 300; i++ {
		tab.Buffer.Lines = append(tab.Buffer.Lines, "// filler")
	}
	tab.ScrollY = 200

	scr := a.screen.(tcell.SimulationScreen)
	a.draw() // must not panic, and must not paint
	scr.Show()
	cells, w, h := scr.GetContents()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			_, bg, _ := cells[y*w+x].Style.Decompose() // (fg, bg, attr)
			if bg == a.theme.ConflictOurs || bg == a.theme.ConflictTheirs || bg == a.theme.ConflictBase {
				t.Fatalf("a scrolled-off region painted a tint at (%d,%d)", x, y)
			}
		}
	}

	// A cache naming lines the buffer no longer has must be survivable too:
	// an external reload can shrink the file between the git tick that filled
	// the cache and the next paint.
	tab.ScrollY = 0
	tab.Buffer.Lines = []string{"just one line now"}
	a.draw() // must not panic or index past the buffer
	scr.Show()
}
