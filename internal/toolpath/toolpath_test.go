// =============================================================================
// File: internal/toolpath/toolpath_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package toolpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLookFindsAnExecutableOffPATH pins the whole point of this package: a herdr
// plugin pane runs with PATH=/usr/bin:/bin:/usr/sbin:/sbin, so a tool installed
// by go, cargo, brew or npm is invisible to exec.LookPath.
func TestLookFindsAnExecutableOffPATH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")

	bin := filepath.Join(home, "go", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(bin, "pretend-server")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Look("pretend-server"); got != tool {
		t.Fatalf("Look = %q, want %q — a tool in ~/go/bin is invisible in a herdr pane", got, tool)
	}
}

// TestLookIgnoresNonExecutablesAndDirectories guards the two things that look
// like a hit and are not: a same-named directory, and a file without +x.
func TestLookIgnoresNonExecutablesAndDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")

	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(filepath.Join(bin, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "noexec"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Look("adir"); got != "" {
		t.Errorf("Look resolved a directory: %q", got)
	}
	if got := Look("noexec"); got != "" {
		t.Errorf("Look resolved a non-executable: %q", got)
	}
}

// TestLookPrefersTheNewestNodeVersion pins the ordering that matters for an
// npm -g install: it lands in whichever node was active, and we cannot know
// which, so the newest wins rather than an arbitrary glob order.
func TestLookPrefersTheNewestNodeVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")

	var newest string
	for _, v := range []string{"v18.0.0", "v24.16.0"} {
		d := filepath.Join(home, ".nvm", "versions", "node", v, "bin")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "some-lsp")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		newest = p
	}
	if got := Look("some-lsp"); got != newest {
		t.Fatalf("Look = %q, want the newest node's copy %q", got, newest)
	}
}

// TestOnlyOnePlaceKnowsWhereToolsLive is the machine-checkable form of why this
// package exists. internal/lsp and internal/dap must AGREE about where a tool
// lives -- one finding gopls while the other cannot find dlv in the same
// directory is worse than either being wrong, because nothing would explain it.
// The logic lived in both for exactly one stage before being pulled out here;
// this fails if a third copy appears.
func TestOnlyOnePlaceKnowsWhereToolsLive(t *testing.T) {
	root := "../.."
	var offenders []string
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.Contains(path, "internal/toolpath/") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// The tell is a hardcoded tool directory outside this package.
		for _, needle := range []string{`".nvm", "versions", "node"`, `".cargo", "bin"`} {
			if strings.Contains(string(body), needle) {
				offenders = append(offenders, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("tool-directory knowledge has escaped internal/toolpath into %v — "+
			"two copies WILL drift, and the symptom is one subsystem finding a binary the other cannot",
			offenders)
	}
}
