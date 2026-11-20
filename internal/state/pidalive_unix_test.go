//go:build !windows

// =============================================================================
// File: internal/state/pidalive_unix_test.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

import (
	"os"
	"os/exec"
	"testing"
)

// deadPID runs a trivial process to completion and returns its (now dead) pid.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// TestPidAlive pins the probe against this process, an invalid pid, and a
// child that has been reaped.
func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("own pid reported dead")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Error("non-positive pid reported alive")
	}
	if pidAlive(deadPID(t)) {
		t.Error("reaped child reported alive")
	}
}
