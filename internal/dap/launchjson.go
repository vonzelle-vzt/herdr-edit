// =============================================================================
// File: internal/dap/launchjson.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// launchjson.go reads a project's .vscode/launch.json, which is JSONC — JSON
// with comments and trailing commas — and which encoding/json cannot parse.
//
// 🔴 The stripper below is a CHARACTER SCANNER that tracks whether it is inside
// a string literal, not a regex, and that distinction is the whole file.
//
// The obvious regex is `(^|[^:])//.*$`, which exists to stop `http://` being
// eaten by requiring the slashes not to follow a colon. That guard is a
// coincidence: it works for URLs only because a URL happens to have a colon in
// front of its slashes. Every other `//` inside a string still breaks it —
// a Windows UNC path, a regex like "a//b", or `"program": "${workspaceFolder}"`
// followed by a comment containing a quote. The failure is silent: the stripper
// truncates a line mid-string, the JSON no longer parses, and the user is told
// their launch.json is invalid when it is not.
//
// Being inside a string is not a property any regex can know, so this does not
// use one.
package dap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LaunchConfig is one entry from a launch.json "configurations" array.
type LaunchConfig struct {
	// Name is what the user sees in the picker.
	Name string
	// Type is the adapter it asks for ("go", "python", …).
	Type string
	// Request is "launch" or "attach".
	Request string
	// Args is the whole raw configuration object, passed to the adapter as-is
	// after the editor fills in anything missing. Adapters accept many more
	// keys than we model, and dropping the ones we do not recognise would
	// silently discard the user's settings.
	Args map[string]interface{}
}

// LaunchJSONPath is where VS Code keeps debug configurations, and therefore
// where a user who has ever debugged this project already has them.
const LaunchJSONPath = ".vscode/launch.json"

// LoadLaunchConfigs reads and parses a project's launch.json.
//
// A missing file is not an error — most projects have none — so it returns an
// empty slice and no error. A malformed one IS an error: the user wrote
// something they expect to be used, and silently ignoring it would leave them
// wondering why their configuration does nothing.
func LoadLaunchConfigs(root string) ([]LaunchConfig, error) {
	path := filepath.Join(root, filepath.FromSlash(LaunchJSONPath))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseLaunchJSON(data)
}

// ParseLaunchJSON parses launch.json content that has already been read.
// Separate from LoadLaunchConfigs so the parsing is testable without a
// filesystem.
func ParseLaunchJSON(data []byte) ([]LaunchConfig, error) {
	var doc struct {
		Version        string                   `json:"version"`
		Configurations []map[string]interface{} `json:"configurations"`
	}
	if err := json.Unmarshal(StripJSONC(data), &doc); err != nil {
		return nil, fmt.Errorf("launch.json: %w", err)
	}
	out := make([]LaunchConfig, 0, len(doc.Configurations))
	for _, cfg := range doc.Configurations {
		out = append(out, LaunchConfig{
			Name:    stringField(cfg, "name"),
			Type:    stringField(cfg, "type"),
			Request: stringField(cfg, "request"),
			Args:    cfg,
		})
	}
	return out, nil
}

// stringField reads a string out of a decoded JSON object, tolerating both a
// missing key and a value of the wrong type.
func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// StripJSONC removes // line comments, /* */ block comments and trailing commas
// from JSONC, leaving string literals untouched.
//
// It is a single left-to-right pass with exactly two pieces of state: whether
// we are inside a string, and whether the previous character inside that string
// was a backslash. That escape tracking is what makes `"C:\\"` end the string at
// the right quote — without it the closing quote reads as escaped, the scanner
// believes it is still inside a string for the rest of the file, and every
// subsequent comment survives into the JSON.
//
// Comments are replaced with nothing rather than with spaces, and newlines
// inside block comments are preserved so that a byte offset in an error message
// still points at roughly the right line.
func StripJSONC(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString := false
	escaped := false

	for i := 0; i < len(src); i++ {
		ch := src[i]

		if inString {
			out = append(out, ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}

		// Outside a string: a quote opens one, a slash may open a comment.
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				// Keep the newline itself: it is whitespace to JSON but it is
				// what keeps line numbers in a parse error meaningful.
				if i < len(src) {
					out = append(out, '\n')
				}
				continue
			}
			if src[i+1] == '*' {
				i += 2
				for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
					if src[i] == '\n' {
						out = append(out, '\n')
					}
					i++
				}
				i++ // land on the '/', the loop's i++ steps past it
				continue
			}
		}

		// A comma is trailing when the next meaningful character closes the
		// object or array it sits in. VS Code accepts those and real
		// launch.json files are full of them, so rejecting one would mean
		// refusing a file the user's other editor opens fine.
		if ch == ',' && nextMeaningfulCloses(src, i+1) {
			continue
		}

		out = append(out, ch)
	}
	return out
}

// nextMeaningfulCloses reports whether the next non-whitespace, non-comment
// character starting at i is a } or ], which is what makes a comma trailing.
//
// It has to skip comments as well as whitespace: `[1, /* last */]` is a
// trailing comma too, and a version that only skipped spaces would leave it in
// place and fail to parse.
func nextMeaningfulCloses(src []byte, i int) bool {
	for i < len(src) {
		switch {
		case src[i] == ' ' || src[i] == '\t' || src[i] == '\r' || src[i] == '\n':
			i++
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i += 2
		default:
			return src[i] == '}' || src[i] == ']'
		}
	}
	return false
}
