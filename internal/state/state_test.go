// =============================================================================
// File: internal/state/state_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// read waits up to a second for the snapshot to appear, so the test tracks the debounce rather
// than guessing a sleep long enough to be flaky on a loaded machine.
func read(t *testing.T, path string) Active {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		blob, err := os.ReadFile(path)
		if err == nil {
			var a Active
			if json.Unmarshal(blob, &a) == nil {
				return a
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no readable snapshot at %s after 1s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newIn(t *testing.T) *Publisher {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewPublisher()
	if p == nil {
		t.Fatal("NewPublisher returned nil with XDG_STATE_HOME set")
	}
	return p
}

func TestPublishesOneBasedPosition(t *testing.T) {
	p := newIn(t)
	// 0-based in, 1-based out: consumers speak file:line:col, and doing the conversion anywhere
	// but here would spread the off-by-one across every reader.
	p.Set("/repo/main.go", 0, 0, "/repo")
	got := read(t, p.path)
	if got.File != "/repo/main.go" || got.Line != 1 || got.Col != 1 || got.Root != "/repo" {
		t.Fatalf("got %+v", got)
	}
	if got.TS == 0 {
		t.Fatal("timestamp not stamped")
	}
}

func TestCoalescesBurstIntoOneWrite(t *testing.T) {
	p := newIn(t)
	// A held arrow key produces hundreds of these. If each wrote, a scroll would be hundreds of
	// syscalls; the debounce must collapse them and still land on the LAST value.
	for i := 0; i < 500; i++ {
		p.Set("/repo/main.go", i, 0, "/repo")
	}
	got := read(t, p.path)
	if got.Line != 500 {
		t.Fatalf("coalesced write should carry the final position, got line %d", got.Line)
	}
}

func TestNoWriteWhenNothingChanged(t *testing.T) {
	p := newIn(t)
	p.Set("/repo/a.go", 3, 4, "/repo")
	first := read(t, p.path)

	st, err := os.Stat(p.path)
	if err != nil {
		t.Fatal(err)
	}
	// Repeating the identical position must not rewrite the file — publishActive runs on EVERY
	// redraw, including pure mouse-move events, so an unconditional write would be constant IO.
	for i := 0; i < 50; i++ {
		p.Set("/repo/a.go", 3, 4, "/repo")
	}
	time.Sleep(2 * debounce)
	st2, err := os.Stat(p.path)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(st.ModTime()) {
		t.Fatal("identical position triggered a rewrite")
	}
	if again := read(t, p.path); again.TS != first.TS {
		t.Fatal("timestamp changed without a position change")
	}
}

func TestEmptyFileWhenNoTabOpen(t *testing.T) {
	p := newIn(t)
	p.Set("/repo/a.go", 1, 1, "/repo")
	read(t, p.path)
	p.Set("", 0, 0, "/repo")
	p.Flush()
	if got := read(t, p.path); got.File != "" {
		t.Fatalf("closing the last tab must publish an empty file, got %q", got.File)
	}
}

func TestFlushBeatsTheDebounce(t *testing.T) {
	p := newIn(t)
	p.Set("/repo/z.go", 9, 9, "/repo")
	p.Flush() // shutdown path: the final position must survive
	blob, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatalf("Flush did not write synchronously: %v", err)
	}
	var a Active
	if err := json.Unmarshal(blob, &a); err != nil || a.Line != 10 {
		t.Fatalf("got %s", blob)
	}
}

func TestNilPublisherIsSafe(t *testing.T) {
	// NewPublisher returns nil when there is no usable state dir; callers must not have to branch.
	var p *Publisher
	p.Set("/x", 1, 1, "/")
	p.Flush()
}

func TestReaderNeverSeesAPartialFile(t *testing.T) {
	p := newIn(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			p.Set("/repo/main.go", i, i, "/repo")
			time.Sleep(time.Millisecond)
		}
		p.Flush()
	}()
	// Hammer the file while it is being rewritten. The temp-file+rename dance means every read
	// either misses the file or gets a complete document — never a truncated one.
	for {
		select {
		case <-done:
			return
		default:
		}
		if blob, err := os.ReadFile(p.path); err == nil && len(blob) > 0 {
			var a Active
			if err := json.Unmarshal(blob, &a); err != nil {
				t.Fatalf("torn read: %v in %q", err, blob)
			}
		}
	}
}

func TestDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
	if got, want := Dir(), filepath.Join("/tmp/xdg", "spiceedit"); got != want {
		t.Fatalf("Dir() = %q want %q", got, want)
	}
}
