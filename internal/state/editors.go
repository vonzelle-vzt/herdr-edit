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
// An entry whose pid is dead (SIGKILL, crash) is pruned on read, so the answer
// stays truthful without any shutdown hook having to run.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/cloudmanic/spice-edit/internal/version"
)

// EditorInfo is one registry entry as published.
type EditorInfo struct {
	PID       int    `json:"pid"`
	Root      string `json:"root"`
	Version   string `json:"version"`
	StartedAt int64  `json:"startedAt"` // unix milliseconds, like every ts here
}

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
// error and a no-op unregister, so callers may defer it unconditionally.
func RegisterEditor(root string) (unregister func(), err error) {
	noop := func() {}
	if Dir() == "" {
		return noop, os.ErrNotExist
	}
	pid := os.Getpid()
	info := EditorInfo{PID: pid, Root: root, Version: version.Version, StartedAt: time.Now().UnixMilli()}
	path := editorFile(pid)
	if err := writeStateJSON(info, ".editor-*.tmp", path); err != nil {
		return noop, err
	}
	return func() { _ = os.Remove(path) }, nil
}

// LiveEditors lists the registered editors whose process is still running,
// ordered by pid. Entries for dead pids, and entries that cannot be parsed, are
// removed as they are found.
func LiveEditors() []EditorInfo {
	entries, err := os.ReadDir(EditorsDir())
	if err != nil {
		return nil
	}
	var out []EditorInfo
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue // temp files from an in-flight write, strays
		}
		path := filepath.Join(EditorsDir(), e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info EditorInfo
		if json.Unmarshal(raw, &info) != nil || info.PID <= 0 || !pidAlive(info.PID) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}
