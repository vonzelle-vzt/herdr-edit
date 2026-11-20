// =============================================================================
// File: internal/dap/livegate_test.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"os"
	"testing"
)

// skipLiveUnderShort keeps `go test -short` a quick loop: the live oracles
// start real tools (some take seconds each), so under -short they skip with a
// loud message naming how to run them. HERDR_REQUIRE_DAP=1 overrides the skip,
// so the anti-skip gate still wins -- CI and anyone who sets it get the full
// oracle (and a failure, not a skip, when the tool is missing). Without -short
// this is a no-op, so the full suite is unchanged.
func skipLiveUnderShort(t *testing.T) {
	t.Helper()
	if testing.Short() && os.Getenv("HERDR_REQUIRE_DAP") != "1" {
		t.Skip("SKIPPED live oracle under -short (real tool, slow); run without -short or set HERDR_REQUIRE_DAP=1 to run it")
	}
}
