// =============================================================================
// File: internal/editor/persist.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// persist.go makes undo history survive closing and reopening a file. The
// in-memory stacks in undo.go die with the process; this file mirrors them
// to disk under $XDG_STATE_HOME/spiceedit/undo (falling back to
// ~/.local/state/spiceedit/undo), one JSON file per absolute file path,
// keyed by a SHA-256 hash of the path so filenames are always safe and
// collision-free.
//
// Safety comes from a content hash stored alongside the history: a
// persisted stack is only ever replayed onto a buffer whose bytes hash to
// exactly the same value the history was recorded against. If the file
// changed on disk in the meantime (edited elsewhere, git checkout, etc.),
// applying stale undo entries could silently corrupt it — so a mismatch
// just means "start fresh," the same as if no history existed.
//
// Every failure path here — an unwritable state dir, a corrupt or
// version-mismatched history file, a path that can't be resolved — is
// swallowed and degrades to in-memory-only undo. This is a convenience
// feature; it must never block editing or destroy the user's work.
package editor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// persistUndoFormatVersion guards against replaying a history file written
// by a future (or incompatible past) version of this format. Bump it any
// time persistedUndo's shape changes in a way old readers couldn't cope
// with; a mismatch is treated exactly like a missing file.
const persistUndoFormatVersion = 1

// Size/entry caps for persisted history. These are vars rather than consts
// so tests can shrink them instead of having to generate megabytes of
// fixture data to exercise the trimming and pruning paths.
var (
	// persistUndoMaxEntries caps how many of the most recent undo/redo
	// entries get written to disk per file, independent of the in-memory
	// maxUndoEntries cap (500) — 500 full-buffer snapshots of a large file
	// would make every keystroke's eventual persist slow and bloat the
	// state directory for no real benefit; the last 50 steps back is
	// already far more than anyone actually undoes through in one sitting.
	persistUndoMaxEntries = 50

	// persistUndoMaxFileBytes caps the serialized size of a single
	// history file. If the capped entry count still doesn't fit (e.g. a
	// handful of enormous snapshots), trimSnapshotsToFit drops the oldest
	// entries until it does.
	persistUndoMaxFileBytes = 4 << 20 // 4 MiB

	// persistUndoMaxTotalBytes caps the combined size of every history
	// file under the undo directory. prunePersistUndoDir deletes the
	// least-recently-written files first once this is exceeded, so the
	// directory can't grow without bound across many opened files.
	persistUndoMaxTotalBytes = 200 << 20 // 200 MiB
)

// persistedSnapshot is the on-disk twin of snapshot (undo.go). It's a
// distinct type (rather than reusing snapshot directly) so the JSON shape
// is decoupled from any future in-memory-only fields added to snapshot.
type persistedSnapshot struct {
	Lines  []string `json:"lines"`
	Cursor Position `json:"cursor"`
	Anchor Position `json:"anchor"`
}

// fromSnapshot converts an in-memory snapshot to its persisted form.
func fromSnapshot(s snapshot) persistedSnapshot {
	return persistedSnapshot{Lines: s.Lines, Cursor: s.Cursor, Anchor: s.Anchor}
}

// toSnapshot converts a persisted snapshot back to the in-memory form used
// by undo.go's stacks.
func (p persistedSnapshot) toSnapshot() snapshot {
	return snapshot{Lines: p.Lines, Cursor: p.Cursor, Anchor: p.Anchor}
}

// toPersistedSnapshots converts a whole undo/redo stack for serialization.
func toPersistedSnapshots(stack []snapshot) []persistedSnapshot {
	out := make([]persistedSnapshot, len(stack))
	for i, s := range stack {
		out[i] = fromSnapshot(s)
	}
	return out
}

// fromPersistedSnapshots converts a deserialized stack back to the
// in-memory snapshot type.
func fromPersistedSnapshots(list []persistedSnapshot) []snapshot {
	out := make([]snapshot, len(list))
	for i, p := range list {
		out[i] = p.toSnapshot()
	}
	return out
}

// persistedUndo is the JSON document written per file. Path is stored only
// for a human reading the file to debug with — ContentHash, not Path, is
// what actually gates whether the history is safe to replay.
type persistedUndo struct {
	Version     int                 `json:"version"`
	Path        string              `json:"path"`
	ContentHash string              `json:"content_hash"`
	SavedAt     time.Time           `json:"saved_at"`
	Undo        []persistedSnapshot `json:"undo"`
	Redo        []persistedSnapshot `json:"redo"`
	Original    persistedSnapshot   `json:"original"`
}

// contentHash returns a hex-encoded SHA-256 digest of data. Used both to
// stamp a persisted history and to check a loaded one against the file's
// current on-disk bytes.
func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// persistUndoDir resolves the directory undo history files live in,
// honoring XDG_STATE_HOME when set and falling back to the XDG default of
// ~/.local/state. Errors only when the home directory can't be resolved
// (no $HOME, no password-database entry) — a genuinely unusual
// environment where persistence should just be skipped.
func persistUndoDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "spiceedit", "undo"), nil
}

// persistUndoKey derives the filename-safe key for absPath: a hex SHA-256
// digest, so arbitrarily weird paths (spaces, unicode, path separators)
// never have to be escaped and can't collide short of a hash collision.
func persistUndoKey(absPath string) string {
	sum := sha256.Sum256([]byte(absPath))
	return hex.EncodeToString(sum[:])
}

// persistUndoFile returns the full path to absPath's history file.
func persistUndoFile(dir, absPath string) string {
	return filepath.Join(dir, persistUndoKey(absPath)+".json")
}

// loadPersistedUndo attempts to resume a previous session's undo history
// for t. diskData is the buffer's just-read on-disk bytes (from NewTab or
// Reload) — comparing against those, rather than re-reading the file
// again, guarantees the hash check is against exactly the content the
// buffer was actually initialised from.
//
// Every failure — no history file, a corrupt one, a version mismatch, or
// (most importantly) a content hash that no longer matches — is handled
// by simply leaving the fresh state that initUndo already set up. This
// must never panic or partially apply a history: the buffer either gets
// the full, verified stack or none of it.
func (t *Tab) loadPersistedUndo(diskData []byte) {
	if t.Path == "" || t.IsImage() {
		return
	}
	abs, err := filepath.Abs(t.Path)
	if err != nil {
		return
	}
	dir, err := persistUndoDir()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(persistUndoFile(dir, abs))
	if err != nil {
		return // no history yet — nothing to resume, not an error
	}
	var rec persistedUndo
	if err := json.Unmarshal(raw, &rec); err != nil {
		return // corrupt file — degrade to fresh in-memory history
	}
	if rec.Version != persistUndoFormatVersion {
		return
	}
	if rec.ContentHash != contentHash(diskData) {
		// The file changed on disk since this history was recorded.
		// Replaying it would splice old line content into the current
		// buffer's undo stack and corrupt it on the first Undo — silently
		// starting fresh is the only safe move.
		return
	}
	t.undoStack = fromPersistedSnapshots(rec.Undo)
	t.redoStack = fromPersistedSnapshots(rec.Redo)
	t.undoOriginal = rec.Original.toSnapshot()
	t.lastUndoGroup = undoGroupNone
}

// PersistUndo writes t's current undo/redo history to disk so it survives
// closing and reopening the tab. It is a convenience, not a guarantee:
// every failure path (unwritable directory, a path that can't be
// resolved, an untitled tab, an image tab) is handled by returning
// early — some paths return a non-nil error for callers that want to log
// it, but none of them leave the in-memory undo stacks in a different
// state, so a failed persist can never cost the user anything beyond "the
// history won't resume next time."
//
// Save() calls this automatically after every successful write, since
// that's the one moment the on-disk bytes and the undo stack are
// guaranteed to agree. UI code that wants history to survive a tab closed
// *without* saving (e.g. after only Undo-able, unsaved edits) should call
// it explicitly on tab close.
func (t *Tab) PersistUndo() error {
	if t.IsImage() || t.Path == "" {
		return nil
	}
	abs, err := filepath.Abs(t.Path)
	if err != nil {
		return err
	}
	// Anchor the hash to what's actually on disk right now, not to
	// t.Buffer — the buffer may be dirty (unsaved edits ahead of disk),
	// and the whole point of the hash is to verify against what a future
	// NewTab will actually read back.
	diskData, err := os.ReadFile(abs)
	if err != nil {
		// Nothing on disk to anchor the history to (e.g. a brand-new,
		// never-saved file) — persisting now would record a hash that
		// can never be verified against later, so there's nothing useful
		// to do.
		return nil
	}
	dir, err := persistUndoDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rec := persistedUndo{
		Version:     persistUndoFormatVersion,
		Path:        abs,
		ContentHash: contentHash(diskData),
		SavedAt:     time.Now(),
		Undo:        toPersistedSnapshots(capForPersist(t.undoStack)),
		Redo:        toPersistedSnapshots(capForPersist(t.redoStack)),
		Original:    fromSnapshot(t.undoOriginal),
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(data) > persistUndoMaxFileBytes {
		// A capped entry count can still overflow the byte budget for a
		// file with very long lines; trim oldest-first rather than refuse
		// to persist at all — a partial history beats none.
		rec.Undo = trimSnapshotsToFit(rec.Undo, persistUndoMaxFileBytes)
		data, err = json.Marshal(rec)
		if err != nil {
			return err
		}
	}
	file := persistUndoFile(dir, abs)
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	// Best-effort directory-wide cap; a pruning failure shouldn't turn a
	// successful persist into a reported error.
	prunePersistUndoDir(dir)
	return nil
}

// capForPersist returns the most recent persistUndoMaxEntries entries of
// stack (or all of it, if shorter). Applied before marshaling so a file
// that has accumulated the full in-memory 500-entry stack doesn't pay to
// serialize and write all of it every save.
func capForPersist(stack []snapshot) []snapshot {
	if len(stack) <= persistUndoMaxEntries {
		return stack
	}
	return stack[len(stack)-persistUndoMaxEntries:]
}

// trimSnapshotsToFit drops the oldest entries of entries until its JSON
// encoding fits within maxBytes, or nothing is left. It re-marshals after
// each drop rather than trying to compute an exact byte-per-entry budget
// up front, since entries can vary wildly in size (a one-line edit vs. a
// whole-file paste) — dropping a quarter of what's left each pass still
// converges in a handful of iterations for any realistic history.
func trimSnapshotsToFit(entries []persistedSnapshot, maxBytes int) []persistedSnapshot {
	for len(entries) > 0 {
		b, err := json.Marshal(entries)
		if err != nil || len(b) <= maxBytes {
			break
		}
		drop := len(entries) / 4
		if drop < 1 {
			drop = 1
		}
		entries = entries[drop:]
	}
	return entries
}

// prunePersistUndoDir keeps the undo directory's total size under
// persistUndoMaxTotalBytes by deleting the least-recently-written history
// files first — those belong to files the user hasn't touched in the
// longest, so they're the least likely to still be useful. Best-effort:
// any error here is swallowed, since a pruning failure shouldn't make the
// persist that triggered it (or editing in general) fail.
func prunePersistUndoDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileStat struct {
		path    string
		size    int64
		modTime time.Time
	}
	var files []fileStat
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		files = append(files, fileStat{
			path:    filepath.Join(dir, e.Name()),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	if total <= int64(persistUndoMaxTotalBytes) {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, f := range files {
		if total <= int64(persistUndoMaxTotalBytes) {
			break
		}
		if err := os.Remove(f.path); err != nil {
			continue
		}
		total -= f.size
	}
}
