package main

import "sync"

// rbacWarnGate makes the /readyz RBAC-denial warning EDGE-TRIGGERED: the first
// denial after a healthy state is reported, subsequent probes stay silent, and a
// recovery re-arms the warning (so a later denial is reported again).
//
// kubelet probes /readyz every few seconds, so an unconditional warn would flood
// the log while RBAC is unconfigured (a single log tail becomes almost all
// warning lines), burying real signals. This mirrors the sandboxreaper's
// "report the first failure loudly, then stay quiet" pattern.
//
// The handler is invoked concurrently (kubelet + any other prober), so the gate
// is safe for concurrent use.
type rbacWarnGate struct {
	mu     sync.Mutex
	warned bool
}

// warnOnce reports a denial exactly on the edge into the denied state: it invokes
// emit and returns true the first time it is called while a denial persists, and
// stays silent (returns false) for every further call until clear is called. The
// emit callback is supplied by the caller so the edge logic is testable without
// depending on the global logger.
func (g *rbacWarnGate) warnOnce(emit func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.warned {
		return false
	}
	g.warned = true
	emit()
	return true
}

// clear resets the gate after a healthy observation, re-arming warnOnce for a
// future denial. It returns true only on the edge out of the denied state (a
// previous denial was recorded), so a caller can log the recovery point once.
func (g *rbacWarnGate) clear() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.warned {
		return false
	}
	g.warned = false
	return true
}
