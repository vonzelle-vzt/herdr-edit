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
	"strconv"
	"strings"
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

// ReadOpenRequest returns the pending request, or ok=false when there is none
// or it cannot be parsed. A malformed file is treated as absent rather than as
// an error: a panel writing junk must not be able to break the editor.
func ReadOpenRequest() (OpenRequest, bool) {
	raw, err := os.ReadFile(OpenRequestFile())
	if err != nil {
		return OpenRequest{}, false
	}
	var req OpenRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return OpenRequest{}, false
	}
	if req.File == "" || req.Seq == 0 {
		return OpenRequest{}, false
	}
	return req, true
}
