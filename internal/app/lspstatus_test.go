// =============================================================================
// File: internal/app/lspstatus_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// TestLSPStatusTextNoManager pins the case app.New never reaches for a
// project with no matching language at all: lspStatusText must return "",
// not panic, when a.lsp is nil.
func TestLSPStatusTextNoManager(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if got := a.lspStatusText(); got != "" {
		t.Fatalf("nil manager: got %q, want empty", got)
	}
}

// writeFakeGopls drops an executable named "gopls" into root's
// node_modules/.bin, which is exactly where Manager.resolve looks before
// falling back to $PATH — so the test never depends on a real language
// server being installed on the machine running it.
func writeFakeGopls(t *testing.T, root, script string) {
	t.Helper()
	binDir := filepath.Join(root, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(binDir, "gopls")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestLSPStatusTextInstalledNotRunning pins the "lsp?" hint: a configured
// server that resolves to a real binary but has never been started (no file
// of its language has been opened yet) is a different situation than no
// server at all, and the status bar tag has to say so without spawning
// anything — Available only stats the path.
func TestLSPStatusTextInstalledNotRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake server script assumes a POSIX shell")
	}
	root := t.TempDir()
	writeFakeGopls(t, root, "#!/bin/sh\nexit 0\n")
	a := newTestApp(t, root)
	a.lsp = lsp.NewManager(root)
	if got := a.lspStatusText(); got != "lsp?" {
		t.Fatalf("installed-not-running: got %q, want %q", got, "lsp?")
	}
}

// fakeLSPServer is a minimal POSIX-sh stand-in for a real language server.
// It speaks just enough Content-Length-framed JSON-RPC to satisfy Client's
// synchronous initialize handshake (echo back whatever numeric "id" it was
// sent, wrapped in an empty result) and exits cleanly on the "exit"
// notification. Spawning a real gopls would make this test depend on the
// machine it runs on; this makes Manager.Running() actually non-empty
// without that dependency.
const fakeLSPServer = `#!/bin/sh
while :; do
  len=""
  while IFS= read -r line; do
    line=$(printf '%s' "$line" | tr -d '\r')
    if [ -z "$line" ]; then
      break
    fi
    case "$line" in
      Content-Length:*) len=$(printf '%s' "$line" | sed 's/^Content-Length: *//') ;;
    esac
  done
  [ -z "$len" ] && exit 0
  body=$(dd bs=1 count="$len" 2>/dev/null)
  case "$body" in
    *'"method":"exit"'*) exit 0 ;;
  esac
  id=$(printf '%s' "$body" | sed -n 's/.*"id" *: *\([0-9][0-9]*\).*/\1/p')
  if [ -n "$id" ]; then
    resp='{"jsonrpc":"2.0","id":'"$id"',"result":{"capabilities":{}}}'
    n=${#resp}
    printf 'Content-Length: %d\r\n\r\n%s' "$n" "$resp"
  fi
done
`

// TestStatusBarShowsRunningServer is the call-site oracle for lspStatusText
// rendered through the real draw path, not just the string helper: it opens
// a .go file against a fake "gopls" so Manager actually has a running
// client, draws the app onto a tcell.SimulationScreen, and reads the
// rendered status row back out. Before lspStatusText/drawStatusBar existed
// (a stub returning "" in their place) this failed because "gopls" never
// appeared anywhere on screen; confirmed by running it against such a stub
// before wiring the real implementation in.
func TestStatusBarShowsRunningServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake server script assumes a POSIX shell")
	}
	root := t.TempDir()
	writeFakeGopls(t, root, fakeLSPServer)

	goFile := filepath.Join(root, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t, root)
	a.lsp = lsp.NewManager(root)
	t.Cleanup(func() { a.lsp.Stop() })

	a.lsp.DidOpen(context.Background(), goFile, "package main\n")
	if running := a.lsp.Running(); len(running) == 0 {
		t.Fatal("fake gopls never came up; nothing for the status bar to show")
	}

	screen := paint(t, a, 120, 40)
	lines := strings.Split(screen, "\n")
	statusRow := lines[len(lines)-2] // draw leaves a trailing blank line from the final \n
	if !strings.Contains(statusRow, "gopls") {
		t.Fatalf("status bar row %q does not mention the running server", statusRow)
	}
}
