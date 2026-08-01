// =============================================================================
// File: internal/editor/tab.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package editor

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/langconf"
	"github.com/cloudmanic/spice-edit/internal/theme"
)

// defaultGutterWidth is the line-number column width for files up to 9999
// lines: five digits plus a one-cell pad on the right, with the git
// change-bar sitting in the blank cell at the far-left of the right-aligned
// number. Larger files grow the gutter via gutterWidthFor so the marker
// never overlaps the first digit.
const defaultGutterWidth = 6

// gutterWidthFor returns the line-number column width for a buffer of
// lineCount lines. It keeps defaultGutterWidth for files that fit and grows
// by one cell per extra digit so the git change-bar always has a blank
// leading cell to sit in. Without this, a 10000-line file would render
// "10000" as "▌0000" with the bar overwriting the first digit, because the
// right-aligned number fills every cell the marker shares.
func gutterWidthFor(lineCount int) int {
	if lineCount <= 0 {
		return defaultGutterWidth
	}
	if w := len(strconv.Itoa(lineCount)) + 2; w > defaultGutterWidth {
		return w
	}
	return defaultGutterWidth
}

// autoClosePairs maps an "opening" character InsertRune sees to the
// character it should auto-insert immediately after it. Quotes map to
// themselves because the opener and closer are the same rune — the
// step-over and word-boundary rules in InsertRune / shouldAutoClose are
// what keep that from producing duplicate quotes.
//
// This is now the FALLBACK, used for file types internal/langconf does not
// cover. The per-language mode this table's comment used to promise is built:
// pairsFor / closersFor / surroundPairsFor below consult the language first,
// and land here only when there is nothing to consult. Keeping it means a
// `.wat` or `.zig` file behaves exactly as it did before per-language data
// existed, rather than losing auto-close outright.
var autoClosePairs = map[rune]rune{
	'(':  ')',
	'[':  ']',
	'{':  '}',
	'"':  '"',
	'\'': '\'',
	'`':  '`',
}

// autoCloseClosers is the reverse index of autoClosePairs' values: the set
// of characters InsertRune should treat as "step over the existing one"
// when they're typed right where they already sit. Built once at package
// init instead of scanning autoClosePairs on every keystroke.
var autoCloseClosers = buildAutoCloseClosers()

// buildAutoCloseClosers derives autoCloseClosers from autoClosePairs so
// the two tables can never drift out of sync with each other.
func buildAutoCloseClosers() map[rune]bool {
	m := make(map[rune]bool, len(autoClosePairs))
	for _, c := range autoClosePairs {
		m[c] = true
	}
	return m
}

// pairsFor returns the auto-closing pairs for this tab's file type: the
// language's own if internal/langconf covers it, otherwise the global
// fallback.
//
// 🔴 Rust is the reason this exists. `'a` is a LIFETIME, not a string, so
// upstream's Rust configuration omits `'` from its auto-closing pairs — the
// old single global table pasted a closing quote into every generic bound.
//
// The returned map belongs to langconf and must not be mutated; it is shared
// by every tab of that language.
func (t *Tab) pairsFor() map[rune]rune {
	if pairs, ok := langconf.AutoClosePairs(t.Path); ok {
		return pairs
	}
	return autoClosePairs
}

// closersFor returns the set of closing runes for this tab's file type, used
// by the step-over rule. Derived from the same language data as pairsFor, so
// a language that does not pair a character also does not step over it —
// stepping over a rune we never inserted would swallow the keystroke.
func (t *Tab) closersFor() map[rune]bool {
	if closers, ok := langconf.AutoCloseClosers(t.Path); ok {
		return closers
	}
	return autoCloseClosers
}

// surroundPairsFor returns the pairs that wrap a selection rather than
// replacing it.
//
// These are a SEPARATE upstream table from the auto-closing pairs and the
// difference is real: Rust surrounds a selection with `<`/`>` for generics,
// but must never auto-close a typed `<`, which would pair every less-than in
// the file. The fallback is the global pair table, which is what surrounding
// used before this data existed.
func (t *Tab) surroundPairsFor() map[rune]rune {
	if pairs, ok := langconf.SurroundPairs(t.Path); ok {
		return pairs
	}
	return autoClosePairs
}

// GitLineChange describes the marker rendered in the editor gutter for a line.
type GitLineChange int

const (
	GitLineNone GitLineChange = iota
	GitLineModified
	GitLineAdded
	GitLineDeleted
)

// Tab is a single open file. It owns the on-disk path, the in-memory buffer,
// the per-tab view state (scroll position, cursor, selection anchor), the
// cached syntax-highlight styles, and a dirty flag.
type Tab struct {
	Path    string // Empty for an unsaved/scratch tab.
	Buffer  *Buffer
	Cursor  Position // Where new typed text appears.
	Anchor  Position // Selection anchor; equals Cursor when nothing is selected.
	ScrollY int      // Index of the first visible line.
	ScrollX int      // Index of the first visible column (rune-indexed). Always 0 when Wrap.

	// Wrap reflows long lines onto extra screen rows instead of letting them run off to the right.
	// Off by default, matching VS Code, and it gates an entirely separate geometry path (wrap.go) --
	// with Wrap false none of the original one-line-per-row arithmetic changes at all.
	Wrap bool

	// ScrollSub is how many of ScrollY's wrapped rows are scrolled off the top. Only meaningful when
	// Wrap is set. Without it a single line longer than the viewport could not be scrolled through:
	// you would see its first screenful and have no way to reach the rest.
	ScrollSub  int
	Dirty      bool
	Styles     [][]tcell.Style
	StyleStale bool
	GitLines   map[int]GitLineChange

	// GitUnmerged is git's verdict — `git ls-files -u` listed this path — and
	// it is the ONLY thing that authorises conflict detection on this buffer.
	// 🔴 A `<<<<<<<` inside a string literal is byte-for-byte a real marker, so
	// no test on the buffer can tell them apart; see conflict.go's header.
	GitUnmerged bool

	// Conflicts is the RENDER CACHE of this tab's merge-conflict regions,
	// refreshed by RescanConflicts. The gutter and the body tint read it; the
	// resolve methods rescan rather than trusting it, because the buffer can
	// be edited between the paint that filled it and the keystroke that acts.
	Conflicts []ConflictRegion

	// Marks holds this tab's gutter marks (breakpoints, logpoints, the
	// future adapter's stopped line), keyed by 0-based buffer line. See
	// marks.go — SetMark/ClearMark/MarkAt/MarkLines are the public surface,
	// and bufInsert/bufDelete below are the only two places lines actually
	// move, so they're the only callers that renumber this map.
	Marks map[int]Mark

	// lastHighlightScrollY / lastHighlightHeight record the viewport Render
	// last tokenised for. Without them, every redraw (mouse moves included)
	// would re-tokenise the visible rows even when nothing changed. Render
	// recomputes only when the content changed (StyleStale) or the viewport
	// shifted (scroll / height), since the grid is indexed by absolute line
	// number and only carries the visible rows.
	lastHighlightScrollY int
	lastHighlightHeight  int

	// lastWrapContentW is the content width Render last drew at. Scroll and the app's wheel handler
	// have no rect, and wrapped scrolling needs one to know how many rows a line occupies.
	lastWrapContentW int

	// Mtime is the file's modification time as of the last successful
	// read or write. The app's periodic disk-reconcile loop compares it
	// against the live mtime to detect external edits.
	Mtime time.Time

	// DiskGone is set when the most recent disk check found the file
	// missing. It exists so we only flash the "deleted on disk" warning
	// once, instead of re-flashing every reconcile tick.
	DiskGone bool

	// cursorMoved is set by every method that changes Cursor; Render
	// consumes it to decide whether to scroll the viewport so the cursor
	// is visible. Without this flag, mouse-wheel scrolling is fought by
	// every redraw — EnsureVisible would snap us back to the cursor.
	cursorMoved bool

	// Undo / redo stacks plus the original on-open snapshot used by
	// RevertFile. See undo.go for the push / coalescing rules and the
	// public Undo / Redo / RevertFile entry points.
	undoStack     []snapshot
	redoStack     []snapshot
	undoOriginal  snapshot
	lastUndoGroup undoGroup
	lastUndoAt    time.Time

	// Mode is "" for a normal text tab and imageMode (= "image") for a
	// read-only image preview. Image tabs reuse the Tab type so the
	// app's tab list, switcher, and modal-routing all just work — the
	// content-mutating methods short-circuit on imageMode and Render
	// delegates to renderImage. See image.go for the render path.
	Mode     string
	Image    image.Image // populated when Mode == imageMode
	ImageFmt string      // "png" / "jpeg" / "gif" — for the status bar

	// Find state — populated when the user opens the find bar and
	// types a query. The UI layer (App) owns the bar geometry and
	// keystroke routing; the tab owns the query, the resolved match
	// list, and the index of the "current" match so the query
	// survives switching tabs and re-opening the bar.
	FindQuery   string
	FindMatches []Match
	FindIndex   int // -1 = no current match; otherwise an index into FindMatches.

	// FindCaseSensitive / FindWholeWord / FindRegex are the toggle state
	// for the three FindOptions flags. They live on the tab (not passed
	// per-call) for the same reason FindQuery does — they need to survive
	// switching tabs and re-opening the find bar. FindErr holds the last
	// regex compile error (nil otherwise) so the UI can show *why* a
	// query produced no matches instead of a misleading "0 of 0".
	FindCaseSensitive bool
	FindWholeWord     bool
	FindRegex         bool
	FindErr           error

	// Synthetic marks a tab whose content was GENERATED (the diff view) rather
	// than read from a file. Such a tab has no Path, must never be saved, and
	// highlights by Label instead. See synthetic.go.
	Synthetic bool
	Label     string

	// IndentUnit is the string the editor inserts when the user presses
	// Tab. Detected on file open (DetectIndent) so the editor matches
	// whatever the file already does — a tab-indented Go file gets a
	// real tab; a 2-space-indented file gets two spaces. Mixed-style
	// files take the dominant signal.
	IndentUnit string
}

// NewTab opens path and returns a Tab. If the file does not exist, the tab
// is created with an empty buffer that will be written on first save —
// matching what most editors do when you "open" a brand-new file path.
// When path looks like an image we recognise (PNG / JPEG / GIF), the tab
// is opened in read-only image-preview mode instead of as text.
func NewTab(path string) (*Tab, error) {
	if path != "" && isImageExt(path) {
		return newImageTab(path)
	}
	var data []byte
	var mtime time.Time
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		data = b
		// Record the on-disk mtime so the app can detect external edits
		// later. A missing file leaves mtime as the zero value, which is
		// fine — the reconcile loop handles that case explicitly.
		if info, statErr := os.Stat(path); statErr == nil {
			mtime = info.ModTime()
		}
	}
	t := &Tab{
		Path:       path,
		Buffer:     NewBuffer(string(data)),
		StyleStale: true,
		Mtime:      mtime,
	}
	t.IndentUnit = DetectIndent(t.Buffer.Lines, path)
	// Record the on-open buffer state so RevertFile has somewhere to
	// rewind to even after the user has typed away.
	t.initUndo()
	// Best-effort resume of a previous session's undo history — see
	// persist.go. loadPersistedUndo only installs it when the file's
	// content hash still matches what's on disk right now (data), so a
	// stale or foreign history can never get replayed onto this buffer.
	t.loadPersistedUndo(data)
	return t, nil
}

// newImageTab decodes path as an image and returns a Tab in image
// preview mode. The buffer is left empty (image tabs ignore it) but
// allocated so any code that pokes at t.Buffer doesn't have to nil-check.
func newImageTab(path string) (*Tab, error) {
	img, format, err := decodeImageFile(path)
	if err != nil {
		return nil, err
	}
	var mtime time.Time
	if info, statErr := os.Stat(path); statErr == nil {
		mtime = info.ModTime()
	}
	t := &Tab{
		Path:     path,
		Buffer:   NewBuffer(""),
		Mtime:    mtime,
		Mode:     imageMode,
		Image:    img,
		ImageFmt: format,
	}
	// Capture the empty original snapshot so CanRevert / RevertFile
	// behave sensibly even though image tabs are read-only — they'll
	// just always report "nothing to revert".
	t.initUndo()
	return t, nil
}

// IsImage reports whether the tab is an image-preview, not a text editor.
// Callers use this to skip text-only behaviour (cursor placement, key
// dispatch, save, etc.) without having to know about Mode strings.
func (t *Tab) IsImage() bool {
	return t.Mode == imageMode
}

// DisplayName returns the basename of Path, or "untitled" for unsaved tabs.
func (t *Tab) DisplayName() string {
	if t.Synthetic {
		return t.Label
	}
	if t.Path == "" {
		return "untitled"
	}
	return filepath.Base(t.Path)
}

// Save writes the buffer to disk and clears Dirty. It is an error to call
// Save on an untitled tab — callers should prompt for a path first. Mtime
// is refreshed so the disk-reconcile loop doesn't immediately think the
// file we just wrote was changed by someone else. Image tabs return an
// error since the editor only knows how to read those, not re-encode them.
func (t *Tab) Save() error {
	if t.IsImage() {
		return fmt.Errorf("image tabs are read-only")
	}
	if t.Synthetic {
		return fmt.Errorf("%s is a generated view and cannot be saved", t.Label)
	}
	if t.Path == "" {
		return fmt.Errorf("no path set for tab")
	}
	if err := os.WriteFile(t.Path, []byte(t.Buffer.String()), 0644); err != nil {
		return err
	}
	t.Dirty = false
	t.DiskGone = false
	if info, err := os.Stat(t.Path); err == nil {
		t.Mtime = info.ModTime()
	}
	// Save is a natural logical-step boundary: the next typing burst is
	// clearly a separate intent, so don't let it merge into whatever was
	// in flight before the save.
	t.breakUndoGroup()
	// Best-effort checkpoint of the undo history to disk — see persist.go.
	// A save is the moment the on-disk content and t.undoStack's history
	// are guaranteed to agree, so it's the natural point to snapshot.
	// Deliberately ignored: persistence is a convenience, never allowed to
	// turn a successful Save into a reported failure.
	_ = t.PersistUndo()
	return nil
}

// Reload re-reads the file from disk into the buffer. Cursor and anchor
// are clamped to the new content (so the user keeps roughly their place
// instead of getting snapped to line 0); ScrollY is left alone and gets
// clamped on the next render. Dirty is cleared and the syntax cache is
// invalidated. Image tabs decode the file again instead of replacing
// the text buffer.
func (t *Tab) Reload() error {
	if t.Path == "" {
		return fmt.Errorf("no path set for tab")
	}
	if t.IsImage() {
		img, format, err := decodeImageFile(t.Path)
		if err != nil {
			return err
		}
		info, err := os.Stat(t.Path)
		if err != nil {
			return err
		}
		t.Image = img
		t.ImageFmt = format
		t.Mtime = info.ModTime()
		t.DiskGone = false
		return nil
	}
	data, err := os.ReadFile(t.Path)
	if err != nil {
		return err
	}
	info, err := os.Stat(t.Path)
	if err != nil {
		return err
	}
	t.Buffer = NewBuffer(string(data))
	t.Cursor = t.Buffer.Clamp(t.Cursor)
	t.Anchor = t.Cursor // drop any selection — line indices may have shifted.
	// Marks survive a reload but must be clamped to the new line count. Their
	// tracking works by observing OUR edits (bufInsert/bufDelete); an external
	// rewrite is invisible to that, so a mark can be left pointing past the end
	// of a file that shrank. Dropping those is the honest answer — a breakpoint
	// on a line that no longer exists is not a breakpoint, and keeping it would
	// silently re-anchor it to whatever now occupies that index.
	t.clampMarks()
	t.Dirty = false
	t.DiskGone = false
	t.Mtime = info.ModTime()
	t.StyleStale = true
	t.cursorMoved = true
	// Reload re-establishes "what's on disk" as the new baseline. Any
	// prior undo history is meaningless now (the line indices may have
	// shifted, and the user explicitly asked to take the disk version),
	// so reset both stacks and the revert anchor.
	t.initUndo()
	// A persisted history from a previous session may still apply to
	// *this* on-disk content (e.g. reload after someone else re-saved the
	// exact bytes we already had) — same hash-gated best-effort resume as
	// NewTab.
	t.loadPersistedUndo(data)
	return nil
}

// HasSelection reports whether the tab currently has a non-empty selection.
func (t *Tab) HasSelection() bool {
	return t.Cursor != t.Anchor
}

// SelectionText returns the currently selected text, or "" if nothing is
// selected. The text is always returned in document order.
func (t *Tab) SelectionText() string {
	if !t.HasSelection() {
		return ""
	}
	return t.Buffer.Substring(t.Anchor, t.Cursor)
}

// bufInsert is the ONLY path allowed to call t.Buffer.InsertString — every
// caller in this file goes through it (TestAllBufferMutationsGoThroughMarkWrappers
// enforces that). Inserting text that contains newlines pushes every mark
// below the insertion point down by however many lines were added; a mark
// sitting ON p.Line itself is left alone, since that line still exists
// afterwards, just with different trailing content.
func (t *Tab) bufInsert(p Position, text string) Position {
	pos := t.Buffer.InsertString(p, text)
	if delta := strings.Count(text, "\n"); delta > 0 {
		t.shiftMarks(p.Line+1, delta)
	}
	return pos
}

// bufDelete is the ONLY path allowed to call t.Buffer.DeleteRange — every
// caller in this file goes through it (TestAllBufferMutationsGoThroughMarkWrappers
// enforces that). Lines strictly between the endpoints, plus the endpoint
// line at c, disappear as line entries (DeleteRange folds c's tail onto a's
// line), so any mark sitting on one of THOSE lines dies with them
// (dropMarksIn) before what remains is renumbered upward (shiftMarks).
func (t *Tab) bufDelete(a, c Position) Position {
	pos := t.Buffer.DeleteRange(a, c)
	lo, hi := PosOrdered(a, c)
	if hi.Line > lo.Line {
		t.dropMarksIn(lo.Line+1, hi.Line+1)
		t.shiftMarks(hi.Line+1, -(hi.Line - lo.Line))
	}
	return pos
}

// DeleteSelection removes the selected range and collapses the cursor to the
// start of the selection. A no-op when nothing is selected.
func (t *Tab) DeleteSelection() {
	if t.IsImage() || !t.HasSelection() {
		return
	}
	// Selection deletes are always their own undo step — they can wipe
	// out a lot in one stroke, and merging them into adjacent typing
	// would make the next undo recover content the user thought was
	// just-deleted.
	t.pushUndo(undoGroupStructural)
	pos := t.bufDelete(t.Anchor, t.Cursor)
	t.Cursor = pos
	t.Anchor = pos
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
}

// InsertString inserts s at the cursor (replacing any selection first) and
// advances the cursor past the inserted text. Always recorded as a
// structural undo step — pasted text or "\n" presses shouldn't merge
// with the surrounding typing burst. No-op on image tabs.
func (t *Tab) InsertString(s string) {
	if t.IsImage() {
		return
	}
	if t.HasSelection() {
		// DeleteSelection records its own structural step. Don't push a
		// second one here or the user would have to undo twice to get
		// back to the pre-paste-with-selection state.
		t.DeleteSelection()
	} else {
		t.pushUndo(undoGroupStructural)
	}
	t.Cursor = t.bufInsert(t.Cursor, s)
	t.Anchor = t.Cursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
}

// InsertRune inserts a single typed character at the cursor. Coalesces
// with adjacent runes inside the undo window so a typed word collapses
// into a single undo step rather than one entry per keystroke. No-op
// on image tabs.
//
// Three auto-close behaviours short-circuit the plain insert, checked in
// this order:
//
//  1. A selection plus a surrounding opener wraps the selection instead of
//     replacing it.
//  2. Typing a closer that's already sitting immediately to the right of
//     the cursor steps over it instead of inserting a duplicate.
//  3. Typing an opener with no selection inserts the pair and leaves the
//     cursor between them — unless it's a quote right after a word
//     character (shouldAutoClose), which would turn `don` + `'` + `t`
//     into `don”t`.
//
// Which characters count as openers and closers is PER LANGUAGE — see
// pairsFor / closersFor / surroundPairsFor. Rust does not pair `'` because
// `'a` is a lifetime; Go and Python do.
func (t *Tab) InsertRune(r rune) {
	if t.IsImage() {
		return
	}
	if t.HasSelection() {
		if closer, ok := t.surroundPairsFor()[r]; ok && closer != 0 {
			t.surroundSelection(r, closer)
			return
		}
		// First-rune-after-selection: let DeleteSelection capture the
		// pre-state, then run the insert without a second push.
		t.DeleteSelection()
	} else {
		if t.stepOverAutoClose(r) {
			return
		}
		if closer, ok := t.pairsFor()[r]; ok && t.shouldAutoClose(r) {
			t.insertAutoClosePair(r, closer)
			return
		}
		t.pushUndo(undoGroupTyping)
	}
	t.Cursor = t.bufInsert(t.Cursor, string(r))
	t.Anchor = t.Cursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
}

// shouldAutoClose reports whether typing opener r should insert its
// closing partner. Brackets always auto-close. Quote characters don't
// when the cursor sits right after a word rune, because that's almost
// always an apostrophe in prose or a contraction/possessive next to an
// identifier ("don't", "it's") rather than the start of a string literal
// — auto-closing there would turn "don't" into "don”t" as the user kept
// typing.
func (t *Tab) shouldAutoClose(r rune) bool {
	if r != '"' && r != '\'' && r != '`' {
		return true
	}
	if t.Cursor.Col == 0 {
		return true
	}
	line := []rune(t.Buffer.Lines[t.Cursor.Line])
	if t.Cursor.Col-1 >= len(line) {
		return true
	}
	return !isWordRune(line[t.Cursor.Col-1])
}

// insertAutoClosePair inserts opener r immediately followed by closer and
// leaves the cursor sitting between them, ready for the user to type the
// pair's contents. Recorded as its own coalescing group (typing) so a
// burst of "(foo)" style edits still collapses into one undo step.
func (t *Tab) insertAutoClosePair(r, closer rune) {
	t.pushUndo(undoGroupTyping)
	pos := t.bufInsert(t.Cursor, string(r)+string(closer))
	t.Cursor = Position{Line: pos.Line, Col: pos.Col - 1}
	t.Anchor = t.Cursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
}

// stepOverAutoClose handles typing a closer that's already sitting
// immediately to the right of the cursor: rather than inserting a second
// copy (turning `(|)` into `()|)`), the cursor just moves past the
// existing one. Returns true when it handled the keystroke, in which
// case the caller must not also perform a normal insert.
func (t *Tab) stepOverAutoClose(r rune) bool {
	if !t.closersFor()[r] {
		return false
	}
	line := []rune(t.Buffer.Lines[t.Cursor.Line])
	if t.Cursor.Col >= len(line) || line[t.Cursor.Col] != r {
		return false
	}
	t.Cursor.Col++
	t.Anchor = t.Cursor
	t.cursorMoved = true
	// Stepping over doesn't mutate the buffer, so it's not itself an undo
	// step — but it is an explicit cursor move, same as any other, so the
	// next typing burst should start a fresh coalescing group rather than
	// merging with whatever came before the step-over.
	t.breakUndoGroup()
	return true
}

// surroundSelection wraps the current selection in opener/closer instead
// of replacing it — typing `(` around `x + y` should produce `(x + y)`,
// not delete the selection and insert a lone paren. The originally
// selected text stays selected (now sitting between the inserted pair) so
// a second bracket press can wrap it again, matching the surround-then-
// re-wrap behaviour users expect from editors that support this.
func (t *Tab) surroundSelection(opener, closer rune) {
	selStart, selEnd := PosOrdered(t.Anchor, t.Cursor)
	t.pushUndo(undoGroupStructural)
	// Insert the closer first: it lands at selEnd, a position the opener
	// insert (which lands at or before selStart) can never invalidate.
	// Inserting in the other order would require shifting selEnd by hand
	// whenever selStart and selEnd share a line.
	t.bufInsert(selEnd, string(closer))
	afterOpener := t.bufInsert(selStart, string(opener))
	newCursor := selEnd
	if selStart.Line == selEnd.Line {
		// The opener we just inserted on this line shifted every column
		// at or after selStart.Col by one, including where selEnd's
		// original content now sits.
		newCursor.Col++
	}
	t.Anchor = afterOpener
	t.Cursor = newCursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
}

// Backspace deletes the character before the cursor (or the selection if any).
// Coalesces with adjacent backspaces inside the undo window. When the
// cursor sits directly between an auto-closed pair with nothing typed
// inside yet (`(|)`), both characters are removed in one keystroke instead
// of leaving a dangling closer behind — see deleteEmptyPair. No-op on
// image tabs.
func (t *Tab) Backspace() {
	if t.IsImage() {
		return
	}
	if t.HasSelection() {
		t.DeleteSelection()
		return
	}
	if t.Cursor.Line == 0 && t.Cursor.Col == 0 {
		return
	}
	if t.deleteEmptyPair() {
		return
	}
	t.pushUndo(undoGroupBackspace)
	var prev Position
	if t.Cursor.Col == 0 {
		prev.Line = t.Cursor.Line - 1
		prev.Col = len([]rune(t.Buffer.Lines[prev.Line]))
	} else {
		prev = Position{Line: t.Cursor.Line, Col: t.Cursor.Col - 1}
	}
	t.Cursor = t.bufDelete(prev, t.Cursor)
	t.Anchor = t.Cursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
}

// deleteEmptyPair handles Backspace landing right between an auto-closed
// pair with nothing typed inside it yet: `(|)` becomes `|` in one
// keystroke instead of leaving a dangling, unmatched closer behind.
// Returns true when it handled the deletion, in which case the caller
// must not also run the normal single-char Backspace path.
func (t *Tab) deleteEmptyPair() bool {
	if t.Cursor.Col == 0 {
		return false
	}
	line := []rune(t.Buffer.Lines[t.Cursor.Line])
	if t.Cursor.Col >= len(line) {
		return false
	}
	before := line[t.Cursor.Col-1]
	after := line[t.Cursor.Col]
	closer, ok := t.pairsFor()[before]
	if !ok || closer != after {
		return false
	}
	t.pushUndo(undoGroupBackspace)
	start := Position{Line: t.Cursor.Line, Col: t.Cursor.Col - 1}
	end := Position{Line: t.Cursor.Line, Col: t.Cursor.Col + 1}
	t.Cursor = t.bufDelete(start, end)
	t.Anchor = t.Cursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
	return true
}

// Delete removes the character after the cursor (or the selection if any).
// Coalesces with adjacent forward-deletes inside the undo window. No-op
// on image tabs.
func (t *Tab) Delete() {
	if t.IsImage() {
		return
	}
	if t.HasSelection() {
		t.DeleteSelection()
		return
	}
	end := t.Buffer.EndPos()
	if t.Cursor == end {
		return
	}
	t.pushUndo(undoGroupDelete)
	var next Position
	line := []rune(t.Buffer.Lines[t.Cursor.Line])
	if t.Cursor.Col >= len(line) {
		next = Position{Line: t.Cursor.Line + 1, Col: 0}
	} else {
		next = Position{Line: t.Cursor.Line, Col: t.Cursor.Col + 1}
	}
	t.Cursor = t.bufDelete(t.Cursor, next)
	t.Anchor = t.Cursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
}

// MoveCursor shifts the cursor by (dLine, dCol). When extend is true the
// anchor is left in place so the user is extending a selection.
func (t *Tab) MoveCursor(dLine, dCol int, extend bool) {
	cur := t.Cursor
	if dLine != 0 {
		cur.Line += dLine
		if cur.Line < 0 {
			cur.Line = 0
		}
		if cur.Line >= t.Buffer.LineCount() {
			cur.Line = t.Buffer.LineCount() - 1
		}
		runes := []rune(t.Buffer.Lines[cur.Line])
		if cur.Col > len(runes) {
			cur.Col = len(runes)
		}
	}
	if dCol != 0 {
		cur.Col += dCol
		if cur.Col < 0 {
			// Wrap to the end of the previous line.
			if cur.Line > 0 {
				cur.Line--
				cur.Col = len([]rune(t.Buffer.Lines[cur.Line]))
			} else {
				cur.Col = 0
			}
		} else {
			runes := []rune(t.Buffer.Lines[cur.Line])
			if cur.Col > len(runes) {
				if cur.Line < t.Buffer.LineCount()-1 {
					cur.Line++
					cur.Col = 0
				} else {
					cur.Col = len(runes)
				}
			}
		}
	}
	t.Cursor = cur
	if !extend {
		t.Anchor = cur
	}
	t.cursorMoved = true
	// Cursor moved on the user's explicit command — close any open
	// coalescing window so the next typing burst is a fresh undo step.
	t.breakUndoGroup()
}

// MoveCursorTo sets the cursor to a specific buffer position. Position is
// clamped within the buffer; extend=true preserves the selection anchor.
func (t *Tab) MoveCursorTo(p Position, extend bool) {
	p = t.Buffer.Clamp(p)
	t.Cursor = p
	if !extend {
		t.Anchor = p
	}
	t.cursorMoved = true
	t.breakUndoGroup()
}

// MoveLineHome moves the cursor to column 0 of the current line.
func (t *Tab) MoveLineHome(extend bool) {
	t.Cursor.Col = 0
	if !extend {
		t.Anchor = t.Cursor
	}
	t.cursorMoved = true
	t.breakUndoGroup()
}

// MoveLineEnd moves the cursor to the last column of the current line.
func (t *Tab) MoveLineEnd(extend bool) {
	t.Cursor.Col = len([]rune(t.Buffer.Lines[t.Cursor.Line]))
	if !extend {
		t.Anchor = t.Cursor
	}
	t.cursorMoved = true
	t.breakUndoGroup()
}

// SelectAll selects the entire buffer (anchor at start, cursor at end).
func (t *Tab) SelectAll() {
	t.Anchor = Position{Line: 0, Col: 0}
	t.Cursor = t.Buffer.EndPos()
	t.cursorMoved = true
	t.breakUndoGroup()
}

// EnsureVisible scrolls the viewport so the cursor is on screen. The
// caller passes the editor area's width and height because the Tab itself
// doesn't know its render rect.
func (t *Tab) EnsureVisible(viewW, viewH int) {
	contentW := viewW - gutterWidthFor(t.Buffer.LineCount()) - 1
	if contentW < 1 {
		contentW = 1
	}
	if t.Wrap {
		t.wrapEnsureVisible(contentW, viewH)
		return
	}
	if t.Cursor.Line < t.ScrollY {
		t.ScrollY = t.Cursor.Line
	}
	if t.Cursor.Line >= t.ScrollY+viewH {
		t.ScrollY = t.Cursor.Line - viewH + 1
	}
	if t.Cursor.Col < t.ScrollX {
		t.ScrollX = t.Cursor.Col
	}
	if t.Cursor.Col >= t.ScrollX+contentW {
		t.ScrollX = t.Cursor.Col - contentW + 1
	}
	if t.ScrollY < 0 {
		t.ScrollY = 0
	}
	if t.ScrollX < 0 {
		t.ScrollX = 0
	}
}

// Render draws the editor's content (line numbers, code with syntax
// highlighting, selection, cursor) into the rectangle (x, y, w, h).
// Image tabs delegate to renderImage instead of drawing text.
func (t *Tab) Render(scr tcell.Screen, th theme.Theme, x, y, w, h int) {
	if t.IsImage() {
		t.renderImage(scr, th, x, y, w, h)
		return
	}
	if t.Wrap {
		t.renderWrapped(scr, th, x, y, w, h)
		return
	}
	// Only re-center on the cursor if the cursor moved this tick. Doing it
	// every render fights the user when they scroll with the wheel.
	if t.cursorMoved {
		t.EnsureVisible(w, h)
		t.cursorMoved = false
	}
	t.clampScroll(h)
	// Re-tokenise only when the content changed (StyleStale) or the viewport
	// shifted (scroll / height). Otherwise every redraw, including mouse
	// moves, would re-tokenise the visible rows for nothing. The grid is
	// indexed by absolute line number and only carries the visible rows, so
	// a scroll change means different rows must be filled.
	if t.StyleStale || t.ScrollY != t.lastHighlightScrollY || h != t.lastHighlightHeight {
		t.Styles = HighlightVisible(t.HighlightKey(), t.Buffer.Lines, t.ScrollY, h, th)
		t.StyleStale = false
		t.lastHighlightScrollY = t.ScrollY
		t.lastHighlightHeight = h
	}

	bg := th.BG
	bgStyle := tcell.StyleDefault.Background(bg).Foreground(th.Text)

	// Paint the entire editor rectangle with the base background first so
	// any cells we don't draw (short lines, blank rows) still get themed.
	for cy := y; cy < y+h; cy++ {
		for cx := x; cx < x+w; cx++ {
			scr.SetContent(cx, cy, ' ', nil, bgStyle)
		}
	}

	selStart, selEnd := PosOrdered(t.Anchor, t.Cursor)
	hasSel := t.HasSelection()

	gw := gutterWidthFor(t.Buffer.LineCount())
	contentX := x + gw + 1
	contentW := w - gw - 1
	if contentW < 1 {
		contentW = 1
	}

	for row := 0; row < h; row++ {
		lineIdx := t.ScrollY + row
		if lineIdx >= t.Buffer.LineCount() {
			break
		}
		cy := y + row
		isCursorLine := lineIdx == t.Cursor.Line

		// Pick the row background — a hair lighter on the cursor's line so
		// the eye can catch where the caret is from across the screen.
		lineBg := bg
		if isCursorLine {
			lineBg = th.LineHL
		}
		lineBgStyle := tcell.StyleDefault.Background(lineBg).Foreground(th.Text)

		// Re-paint this row with its (possibly highlighted) bg.
		for cx := x; cx < x+w; cx++ {
			scr.SetContent(cx, cy, ' ', nil, lineBgStyle)
		}

		// Gutter / line number, right-aligned with one trailing space.
		numStr := fmt.Sprintf("%*d", gw-1, lineIdx+1)
		gutterStyle := tcell.StyleDefault.Background(lineBg).Foreground(th.Muted)
		if isCursorLine {
			gutterStyle = gutterStyle.Foreground(th.AccentSoft)
		}
		markerR, markerColor, hasMarker := t.gutterMarker(th, lineIdx)
		if hasMarker {
			scr.SetContent(x, cy, markerR, nil, gutterStyle.Foreground(markerColor))
		}
		for i, r := range numStr {
			if i == 0 && hasMarker {
				continue
			}
			scr.SetContent(x+i, cy, r, nil, gutterStyle)
		}

		// Line content, with syntax styles, selection bg, and line bg.
		// We walk from the start of the line so tab stops anchor to col 0
		// — a tab one cell into the line still expands to the next stop,
		// not the next-stop-from-the-scroll-offset. ScrollX skips runes;
		// the visual column we paint at is rune-walked from there.
		runes := []rune(t.Buffer.Lines[lineIdx])
		var styles []tcell.Style
		if lineIdx < len(t.Styles) {
			styles = t.Styles[lineIdx]
		}
		scrollVisual := LineVisualCol(runes, t.ScrollX)
		visualCol := 0 // visual cell offset from the start of the LINE
		for runeIdx, r := range runes {
			width := RuneVisualWidth(r, visualCol)
			if runeIdx >= t.ScrollX {
				// Once we're past ScrollX, paint each cell of this rune.
				// The rune's first cell shows the actual glyph (or ' '
				// for tabs); padding cells for a multi-cell tab show a
				// space so the trailing tab area still gets the right bg.
				st := bgStyle
				if runeIdx < len(styles) {
					st = styles[runeIdx]
				}
				st = st.Background(lineBg)
				if hasSel {
					p := Position{Line: lineIdx, Col: runeIdx}
					if !PosLess(p, selStart) && PosLess(p, selEnd) {
						st = st.Background(th.Selection)
					}
				}
				if mIdx := t.matchAtRune(lineIdx, runeIdx); mIdx >= 0 {
					if mIdx == t.FindIndex {
						st = st.Background(th.FindCurrent).Foreground(th.BG)
					} else {
						st = st.Background(th.FindMatch)
					}
				}
				glyph := r
				if r == '\t' {
					glyph = ' '
				}
				for cell := 0; cell < width; cell++ {
					sc := visualCol - scrollVisual + cell
					if sc < 0 {
						continue
					}
					if sc >= contentW {
						break
					}
					ch := glyph
					if cell > 0 {
						ch = ' '
					}
					scr.SetContent(contentX+sc, cy, ch, nil, st)
				}
			}
			visualCol += width
		}

		// Overflow affordance: paint a muted '‹' / '›' over the leftmost /
		// rightmost content cell when the line extends past the viewport
		// in that direction. Without this hint a terminal user has no way
		// to tell that more content exists off-screen — there's no
		// scrollbar to clue them in. visualCol now equals the total
		// visual width of the line; scrollVisual is the visual cell
		// corresponding to ScrollX.
		overflowStyle := tcell.StyleDefault.Background(lineBg).Foreground(th.Muted)
		if t.ScrollX > 0 {
			scr.SetContent(contentX, cy, '‹', nil, overflowStyle)
		}
		if visualCol-scrollVisual > contentW {
			scr.SetContent(contentX+contentW-1, cy, '›', nil, overflowStyle)
		}
	}

	// Position the hardware cursor at its visual column (so a cursor
	// past a tab lands at the tab's *end* cell, not just rune-Col cells
	// to the right of ScrollX).
	cy := y + (t.Cursor.Line - t.ScrollY)
	cursorRunes := t.Buffer.LineRunes(t.Cursor.Line)
	cursorVisual := LineVisualCol(cursorRunes, t.Cursor.Col)
	scrollVisual := LineVisualCol(cursorRunes, t.ScrollX)
	cx := contentX + (cursorVisual - scrollVisual)
	if cy >= y && cy < y+h && cx >= contentX && cx < contentX+contentW {
		scr.ShowCursor(cx, cy)
	} else {
		scr.HideCursor()
	}
}

// gitLineMarkerRune returns the gutter glyph for a git line change.
func gitLineMarkerRune(change GitLineChange) rune {
	if change == GitLineDeleted {
		return '▁'
	}
	return '▌'
}

// gitLineMarkerColor returns the gutter color for a git line change.
func gitLineMarkerColor(th theme.Theme, change GitLineChange) tcell.Color {
	if change == GitLineAdded {
		return th.GitAdded
	}
	if change == GitLineDeleted {
		return th.GitDeleted
	}
	return th.GitModified
}

// HitTest converts screen coordinates within this tab's render area to a
// buffer position. ok=false means the click was outside any line.
func (t *Tab) HitTest(localX, localY, w, h int) (Position, bool) {
	if localY < 0 || localY >= h {
		return Position{}, false
	}
	contentX := gutterWidthFor(t.Buffer.LineCount()) + 1
	if t.Wrap {
		contentW := w - contentX
		if contentW < 1 {
			contentW = 1
		}
		lineIdx, segIdx, ok := t.wrapRowAt(localY, contentW)
		if !ok {
			return Position{}, false
		}
		if localX < contentX {
			return Position{Line: lineIdx, Col: t.lineSegments(lineIdx, contentW)[segIdx].Start}, true
		}
		col := t.colAtSegmentVisual(lineIdx, segIdx, localX-contentX, contentW)
		return Position{Line: lineIdx, Col: col}, true
	}
	line := t.ScrollY + localY
	if line < 0 || line >= t.Buffer.LineCount() {
		return Position{}, false
	}
	if localX < contentX {
		// Click in the gutter — treat as click at column 0 of that line.
		return Position{Line: line, Col: 0}, true
	}
	runes := []rune(t.Buffer.Lines[line])
	// Convert the click's screen column back to a rune column. With tabs
	// expanding to multi-cell tab stops we can't just subtract ScrollX
	// from localX — we have to walk the runes counting visual cells.
	scrollVisual := LineVisualCol(runes, t.ScrollX)
	targetVisual := scrollVisual + (localX - contentX)
	col := RuneColAtVisual(runes, targetVisual)
	if col > len(runes) {
		col = len(runes)
	}
	if col < 0 {
		col = 0
	}
	return Position{Line: line, Col: col}, true
}

// Scroll moves the viewport by delta lines (negative = up). Render runs
// clampScroll afterwards so the user never scrolls into pure void; here we
// just adjust the raw value.
func (t *Tab) Scroll(deltaLines int) {
	if t.Wrap {
		// contentW is not known here, so use the last width Render saw. A wheel notch should travel
		// the same visual distance whether the lines under it wrap or not.
		t.wrapScroll(deltaLines, t.lastWrapContentW)
		return
	}
	t.ScrollY += deltaLines
	if t.ScrollY < 0 {
		t.ScrollY = 0
	}
}

// ScrollH moves the viewport horizontally by delta rune-columns (negative
// = left). Clamped at zero; the right side is naturally bounded by
// Render's contentW window — scrolling past the longest visible line just
// shows blank space, which is fine. Lives next to Scroll so the app's
// mouse-wheel dispatcher can treat horizontal and vertical wheels
// symmetrically.
func (t *Tab) ScrollH(deltaCols int) {
	t.ScrollX += deltaCols
	if t.ScrollX < 0 {
		t.ScrollX = 0
	}
}

// clampScroll keeps ScrollY inside a sensible range for the current viewport
// height. The max is "last line still on screen, plus a small overscroll
// pad" so the user can scroll the bottom of the file up to the middle of
// the viewport — which feels much better than abruptly stopping when the
// last line hits the bottom row.
func (t *Tab) clampScroll(viewH int) {
	if t.Wrap {
		// contentW is unknown here; callers that need the wrapped clamp use wrapClampScroll directly
		// from Render, where the rect is known. Just keep the raw value sane.
		if t.ScrollY < 0 {
			t.ScrollY = 0
		}
		return
	}
	if t.ScrollY < 0 {
		t.ScrollY = 0
	}
	overscroll := viewH / 2
	if overscroll < 3 {
		overscroll = 3
	}
	max := t.Buffer.LineCount() - viewH + overscroll
	if max < 0 {
		max = 0
	}
	if t.ScrollY > max {
		t.ScrollY = max
	}
}
