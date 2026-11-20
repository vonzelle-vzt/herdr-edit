//go:build !windows

// =============================================================================
// File: internal/state/pidalive_unix.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

import (
	"errors"
	"syscall"
)

// pidAlive reports whether a process with this pid exists. Signal 0 delivers
// nothing and only checks; EPERM means it exists but belongs to someone else,
// which still counts as alive.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
