// =============================================================================
// File: internal/lsp/livegate_test.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package lsp

import (
	"os"
	"testing"
)

// skipLiveUnderShort keeps `go test -short` a quick loop: the live oracles
// start real language servers, so under -short they skip with a loud message
// naming how to run them. HERDR_REQUIRE_LSP=1 overrides the skip, so the
// anti-skip gate still wins. Without -short this is a no-op, so the full suite
// is unchanged.
func skipLiveUnderShort(t *testing.T) {
	t.Helper()
	if testing.Short() && os.Getenv("HERDR_REQUIRE_LSP") != "1" {
		t.Skip("SKIPPED live oracle under -short (real server, slow); run without -short or set HERDR_REQUIRE_LSP=1 to run it")
	}
}
