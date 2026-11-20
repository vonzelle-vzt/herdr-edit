// =============================================================================
// File: internal/state/openreq.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// openreq.go is the REVERSE of the active-file contract.
//
// active.json flows editor -> panels: "here is the file and line I am looking
// at", so the Blame, Markdown and Problems panels can follow the cursor.
// open-request.json flows panels -> editor: "open this file at this line".
//
// That direction is what turns a read-only review into an edit. Every competing
// herdr plugin — file-viewer, reviewr, sidebar — can show you a diff and stop
// there; the Review panel can hand a `path:line` straight to a real editor with
// a language server attached, and you fix it instead of writing a note about it.
//
// The mechanism is deliberately the same shape as the publisher: one small JSON
// file, written with temp-file + rename so a reader never sees a half-written
// payload, and carrying a monotonic sequence so the editor can tell a new
// request from one it has already honoured. A stale request must never reopen a
// file the user closed twenty minutes ago.

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SplitLocation parses "path", "path:line" and "path:line:col" into their
// parts, defaulting line and col to 1.
//
// Splitting from the RIGHT is what makes this correct on the paths that
// actually occur: an absolute path or any filename containing a colon would
// be cut in the wrong place by a left-to-right scan, and the trailing numeric
// fields are the only unambiguous part of the string. Promoted from main.go
// so internal/app can parse a "path:line:col" reference too (menuGoToLocation)
// without a second, drifting copy of this logic.
func SplitLocation(s string) (path string, line, col int) {
	line, col = 1, 1
	parts := strings.Split(s, ":")
	for len(parts) > 1 {
		last := parts[len(parts)-1]
		n, err := strconv.Atoi(last)
		if err != nil || n < 1 {
			break
		}
		if col == 1 && line == 1 {
			line = n
		} else {
			col = line
			line = n
		}
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, ":"), line, col
}

// OpenRequestFile is the path panels write and the editor polls.
func OpenRequestFile() string {
	return filepath.Join(Dir(), "open-request.json")
}

// OpenRequest is one "open this file" instruction. Line and Col are 1-based on
// the wire, matching how every tool prints a location and how a human reads
// one; the editor converts on the way in.
type OpenRequest struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Seq  int64  `json:"seq"`
}

// WriteOpenRequest asks the editor to open file at line/col. Used by the CLI
// that panels shell out to.
func WriteOpenRequest(file string, line, col int) error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	req := OpenRequest{File: file, Line: line, Col: col, Seq: time.Now().UnixNano()}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "open-request-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, OpenRequestFile())
}

// readFile is os.ReadFile behind a seam so tests can count how often a polled
// state file is actually read.
var readFile = os.ReadFile

// racyWindow is how recent a file's mtime may be before a stat match is not
// trusted. A rewrite that keeps the same size and lands inside one filesystem
// timestamp tick would be invisible to (mtime, size, inode); git calls such an
// entry "racily clean" and re-reads it, and so do we. Every writer in this
// package replaces the file by rename, which changes the inode anyway — the
// window only exists to cover a foreign tool that rewrites in place.
const racyWindow = 2 * time.Second

// fileSig identifies one version of a file cheaply. mtime is in nanoseconds so
// two writes a millisecond apart differ; ino is 0 on platforms whose FileInfo
// carries none (Windows), where mtime and size alone decide.
type fileSig struct {
	mtime int64
	size  int64
	ino   uint64
}

// sigOf extracts the signature from a stat result. The inode comes out by
// reflection so this file builds on every platform the release targets
// without a build-tagged sibling: syscall.Stat_t does not exist on Windows.
func sigOf(fi os.FileInfo) fileSig {
	sig := fileSig{mtime: fi.ModTime().UnixNano(), size: fi.Size()}
	if v := reflect.ValueOf(fi.Sys()); v.IsValid() && v.Kind() == reflect.Ptr && !v.IsNil() {
		if f := v.Elem().FieldByName("Ino"); f.IsValid() && f.CanUint() {
			sig.ino = f.Uint()
		}
	}
	return sig
}

// polledFile caches the parsed contents of one state file between polls. The
// editor's Run loop reads open-request.json and debug-request.json on EVERY
// event — mouse motion is 50-100 events a second — and nearly every one of
// those finds the same bytes as the last. A stat is a single syscall; the
// read, allocation and unmarshal it replaces are the cost worth avoiding.
type polledFile[T any] struct {
	mu   sync.Mutex
	path string
	have bool
	sig  fileSig
	val  T
	ok   bool
}

// get returns parse's verdict on the file at path, reusing the previous one
// when (mtime ns, size, inode) are unchanged and the file is not so recent
// that a same-size rewrite could hide. A missing file is never cached: it
// drops the entry so the file reappearing is always read.
func (c *polledFile[T]) get(path string, parse func(raw []byte) (T, bool)) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	fi, err := os.Stat(path)
	if err != nil {
		c.have = false
		return zero, false
	}
	sig := sigOf(fi)
	if c.have && c.path == path && c.sig == sig && time.Since(fi.ModTime()) > racyWindow {
		return c.val, c.ok
	}
	raw, err := readFile(path)
	if err != nil {
		c.have = false
		return zero, false
	}
	val, ok := parse(raw)
	c.path, c.sig, c.val, c.ok, c.have = path, sig, val, ok, true
	return val, ok
}

// openRequestCache is ReadOpenRequest's memory.
var openRequestCache polledFile[OpenRequest]

// ReadOpenRequest returns the pending request, or ok=false when there is none
// or it cannot be parsed. A malformed file is treated as absent rather than as
// an error: a panel writing junk must not be able to break the editor.
//
// The answer is cached against the file's stat signature, so the per-event
// poll in Run costs one stat. The sequence guard is unaffected: the cache
// returns exactly what a re-read would, and a new request always changes the
// signature (WriteOpenRequest renames a fresh file into place).
func ReadOpenRequest() (OpenRequest, bool) {
	return openRequestCache.get(OpenRequestFile(), func(raw []byte) (OpenRequest, bool) {
		var req OpenRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return OpenRequest{}, false
		}
		if req.File == "" || req.Seq == 0 {
			return OpenRequest{}, false
		}
		return req, true
	})
}
