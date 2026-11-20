//go:build windows

// =============================================================================
// File: internal/state/pidalive_windows.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

// pidAlive is the windows fallback: there is no cheap signal-0 probe, so every
// pid reads as dead. The consequence is that an idle write always overwrites
// and dead-editor entries are pruned eagerly, which errs toward a missing
// session rather than a stale one.
func pidAlive(pid int) bool { return false }
