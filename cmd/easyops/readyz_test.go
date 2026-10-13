package main

import (
	"sync"
	"testing"
)

// TestRBACWarnGateEdgeTriggered checks the edge-triggered semantics: the warning
// fires on the first denial, stays silent while the denial persists, re-arms
// after a recovery, and reports the recovery edge once.
func TestRBACWarnGateEdgeTriggered(t *testing.T) {
	var g rbacWarnGate
	var warns, clears int

	// Three consecutive denials -> exactly ONE warn.
	for i := 0; i < 3; i++ {
		if g.warnOnce(func() { warns++ }) {
			// reported
		}
	}
	if warns != 1 {
		t.Fatalf("warns after 3 denials = %d, want 1", warns)
	}
	if g.clear() {
		clears++
	}
	if clears != 1 {
		t.Fatalf("clears after recovery = %d, want 1", clears)
	}
	// A second clear with no denial in between must NOT report a recovery edge.
	if g.clear() {
		t.Fatalf("clear() reported an edge with no prior denial")
	}
	// A later denial re-arms and reports again.
	if !g.warnOnce(func() { warns++ }) {
		t.Fatal("warnOnce did not re-fire after a recovery")
	}
	if warns != 2 {
		t.Fatalf("warns after re-arm = %d, want 2", warns)
	}
}

// TestRBACWarnGateConcurrent exercises the gate from many goroutines (the readyz
// handler is invoked concurrently by probes) to catch a data race and to assert
// that exactly one goroutine reports the first denial.
func TestRBACWarnGateConcurrent(t *testing.T) {
	var g rbacWarnGate
	var mu sync.Mutex
	warns := 0
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.warnOnce(func() {
				mu.Lock()
				warns++
				mu.Unlock()
			})
		}()
	}
	wg.Wait()
	if warns != 1 {
		t.Fatalf("concurrent warns = %d, want exactly 1", warns)
	}
}
