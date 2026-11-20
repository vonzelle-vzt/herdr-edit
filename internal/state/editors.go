// =============================================================================
// File: internal/state/editors.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// editors.go is the liveness registry: one small file per RUNNING editor in
// $STATE/editors/<pid>.json. active.json and debug-session.json each describe
// one editor and are overwritten by whichever wrote last, so neither can answer
// "is any editor running at all?" — which is exactly what a CLI that just wrote
// an open-request needs to know before it tells its caller the request landed.
//
// An entry only counts as live when its pid is alive AND its lastBeat is fresh
// (BeatEditor runs on the editor's 10s tick). The pid test alone is not enough:
// after a crash the OS may hand that pid to an unrelated process, and the stale
// record would then claim an editor that does not exist. A fresh beat proves the
// writer is still the editor; a random per-process Instance id lets a beat tell
// its own record from a stranger's that reused the file name.
//
// Reading never mutates the directory, so --capabilities and the handoff check
// leave state exactly as they found it. Dead and stale entries are pruned when
// an editor registers.
//
// 🔴 What this registry does NOT give you: a consumption acknowledgement, or a
// choice of WHICH editor takes a request. Requests are a broadcast; each editor
// claims only paths inside its own root (withinRoot). Exit 0 from a handoff
// therefore means "queued while a live editor was registered" — never "opened".
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/cloudmanic/spice-edit/internal/version"
)

// EditorInfo is one registry entry as published.
type EditorInfo struct {
	Schema    int    `json:"schema"`
	PID       int    `json:"pid"`
	Instance  string `json:"instance"`
	Root      string `json:"root"`
	Version   string `json:"version"`
	StartedAt int64  `json:"startedAt"` // unix milliseconds, like every ts here
	LastBeat  int64  `json:"lastBeat"`  // unix milliseconds; see EditorStaleAfter
}

// EditorRegistrySchema is the wire version of an EditorInfo record.
const EditorRegistrySchema = 1

// EditorStaleAfter is how old a lastBeat may be before the entry stops counting
// as live. Six missed 10s ticks: generous enough that a busy editor is never
// declared dead, short enough that a recycled pid is not believed for long.
const EditorStaleAfter = 60 * time.Second

// maxEditorRecord bounds how much of an entry LiveEditors will read; a real one
// is a couple of hundred bytes, so anything near this is not ours.
const maxEditorRecord = 4096

// registered is this process's own entry, kept so BeatEditor can refresh it.
var registered struct {
	sync.Mutex
	info *EditorInfo
	path string
}

// nowMillis is the registry clock, a seam so tests can age entries.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

// EditorsDir is where the registry entries live.
func EditorsDir() string {
	return filepath.Join(Dir(), "editors")
}

// editorFile is the entry path for one pid.
func editorFile(pid int) string {
	return filepath.Join(EditorsDir(), strconv.Itoa(pid)+".json")
}

// RegisterEditor records this process as a running editor rooted at root and
// returns the function that removes the entry again. The unregister func is
// safe to call more than once. With no usable state directory it returns an
// error and a no-op unregister, so callers may defer it unconditionally. Dead
// and stale entries left by crashed editors are pruned here.
func RegisterEditor(root string) (unregister func(), err error) {
	noop := func() {}
	if Dir() == "" {
		return noop, os.ErrNotExist
	}
	pruneEditors()
	var id [8]byte
	_, _ = rand.Read(id[:])
	pid := os.Getpid()
	now := nowMillis()
	info := &EditorInfo{
		Schema: EditorRegistrySchema, PID: pid, Instance: hex.EncodeToString(id[:]),
		Root: root, Version: version.Version, StartedAt: now, LastBeat: now,
	}
	path := editorFile(pid)
	if err := writeStateJSON(info, ".editor-*.tmp", path); err != nil {
		return noop, err
	}
	registered.Lock()
	registered.info, registered.path = info, path
	registered.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			registered.Lock()
			if registered.path == path {
				registered.info, registered.path = nil, ""
			}
			registered.Unlock()
			_ = os.Remove(path)
		})
	}, nil
}

// BeatEditor refreshes this process's registry entry so it keeps counting as
// live. Called from the editor's periodic tick; a no-op when unregistered. A
// failed write is returned but harmless: the entry simply ages out, and the CLI
// then reports "no running editor" — the honest answer for an editor whose
// state directory has stopped accepting writes.
func BeatEditor() error {
	registered.Lock()
	defer registered.Unlock()
	if registered.info == nil {
		return nil
	}
	registered.info.LastBeat = nowMillis()
	return writeStateJSON(registered.info, ".editor-*.tmp", registered.path)
}

// LiveEditors lists the registered editors that are running right now —
// pid alive and lastBeat within EditorStaleAfter — ordered by pid. It only
// reads: symlinks, oversized and unparseable entries are skipped, not deleted.
func LiveEditors() []EditorInfo {
	var out []EditorInfo
	for _, e := range scanEditors() {
		if e.live {
			out = append(out, e.info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// editorEntry is one scanned registry file and the verdict on it.
type editorEntry struct {
	path string
	info EditorInfo
	live bool
}

// scanEditors reads every regular *.json file in the registry. Entries it
// cannot trust (a symlink, too large, unparseable) come back with live=false
// so pruneEditors can remove them; LiveEditors just ignores them.
func scanEditors() []editorEntry {
	dir := EditorsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []editorEntry
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue // temp files from an in-flight write, strays
		}
		path := filepath.Join(dir, e.Name())
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			out = append(out, editorEntry{path: path}) // symlink or special file: never followed
			continue
		}
		out = append(out, readEditorEntry(path))
	}
	return out
}

// readEditorEntry parses one registry file and decides whether it is live.
func readEditorEntry(path string) editorEntry {
	ent := editorEntry{path: path}
	f, err := os.Open(path)
	if err != nil {
		return ent
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxEditorRecord+1))
	if err != nil || len(raw) > maxEditorRecord {
		return ent
	}
	if json.Unmarshal(raw, &ent.info) != nil || ent.info.PID <= 0 {
		return ent
	}
	beat := ent.info.LastBeat
	if beat == 0 {
		beat = ent.info.StartedAt
	}
	fresh := nowMillis()-beat < EditorStaleAfter.Milliseconds()
	ent.live = fresh && pidAlive(ent.info.PID)
	return ent
}

// pruneEditors removes every entry that is not live. Run when an editor
// registers, so crash residue is bounded by how often editors start.
func pruneEditors() {
	for _, e := range scanEditors() {
		if !e.live {
			_ = os.Remove(e.path)
		}
	}
}
