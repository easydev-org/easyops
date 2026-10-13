// Package sandboxreaper reclaims idle sandboxes. A sandbox is idle when its
// worker reports NO jobs and the pod has existed longer than the idle TTL.
//
// The worker retains only a 24h job window (jobsvc retentionHours = 24), so
// "no jobs at all" means "no job in the last 24h" — combined with the pod age
// check, a sandbox is reclaimed only after a full idle day. Running jobs and
// any job finished within the window keep it alive.
//
// This package is a pure library: it does NOT import a concrete worker client
// (that lives in the service layer and pulls in connectrpc). The caller injects
// a WorkerLister via NewWorker; the default is nil, which means a sandbox whose
// worker cannot be inspected is never reaped.
package sandboxreaper

import (
	"context"
	"log"
	"time"

	"github.com/easy-platform/easyops/internal/sandboxmgr"
)

// Job is the minimal worker job summary the reaper needs. It mirrors the
// workerclient.Job shape but is declared here so the library stays free of the
// worker protocol (connectrpc / gen/worker).
type Job struct {
	State      string
	FinishedAt int64 // unix millis, 0 while running
}

// Reaper periodically deletes idle sandboxes.
type Reaper struct {
	Sbx      *sandboxmgr.Client
	IdleTTL  time.Duration // idle threshold (default 24h; <=0 disables)
	Interval time.Duration // sweep period (default 10m)
	// NewWorker builds a worker client for a sandbox's resolved url+token. It is
	// REQUIRED for reclamation: when nil, no sandbox can be inspected and none is
	// reaped (the library never links a concrete worker client).
	NewWorker func(url, token string) WorkerLister
	// warnedList records that the FIRST list failure was already reported
	// loudly, so a persistent RBAC denial does not spam the log every sweep.
	warnedList bool
}

// WorkerLister is the subset of a worker client the reaper needs.
type WorkerLister interface {
	ListJobs(ctx context.Context) ([]Job, error)
}

// Run sweeps until ctx is cancelled. A non-positive IdleTTL disables reaping.
func (r *Reaper) Run(ctx context.Context) {
	if r.IdleTTL <= 0 {
		log.Printf("sandbox reaper: disabled (idle TTL <= 0)")
		return
	}
	interval := r.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	log.Printf("sandbox reaper: idle TTL=%s interval=%s", r.IdleTTL, interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.SweepOnce(ctx)
		}
	}
}

// SweepOnce performs one reclamation pass.
func (r *Reaper) SweepOnce(ctx context.Context) {
	sbxs, err := r.Sbx.List(ctx)
	if err != nil {
		// Report the FIRST failure loudly (a missing Role/RoleBinding is the
		// usual cause), then stay quiet so a persistent denial does not spam the
		// log every sweep. A later success re-arms the warning.
		if !r.warnedList {
			log.Printf("warn: sandbox reaper: cannot list sandboxes in ns %q: %v — idle sandboxes will NOT be reclaimed; RBAC likely missing (the Pod must run as the easyops ServiceAccount with the worker-ns Role applied)",
				r.Sbx.Namespace(), err)
			r.warnedList = true
		}
		return
	}
	r.warnedList = false
	now := time.Now().UnixMilli()
	for _, sb := range sbxs {
		if !r.idle(ctx, sb, now) {
			continue
		}
		if ok, err := r.Sbx.Delete(ctx, sb.Name); err != nil {
			log.Printf("sandbox reaper: delete %s: %v", sb.Name, err)
		} else if ok {
			log.Printf("sandbox reaper: reclaimed idle sandbox %q (age %s)", sb.Name, time.Duration(now-sb.CreatedAt)*time.Millisecond)
		}
	}
}

// idle reports whether a sandbox has had no jobs for the idle TTL and is old
// enough to reclaim. An unreachable worker is treated as NOT idle (never reap
// something we cannot inspect).
func (r *Reaper) idle(ctx context.Context, sb sandboxmgr.Sandbox, now int64) bool {
	if sb.CreatedAt == 0 || now-sb.CreatedAt < r.IdleTTL.Milliseconds() {
		return false
	}
	// No worker client: the sandbox cannot be inspected, so it is never reaped.
	if r.NewWorker == nil {
		return false
	}
	url, token, err := r.Sbx.Resolve(ctx, sb.Name)
	if err != nil {
		return false
	}
	jobs, err := r.NewWorker(url, token).ListJobs(ctx)
	if err != nil {
		return false
	}
	return isIdle(sb.CreatedAt, now, r.IdleTTL, jobs)
}

// isIdle is the pure reclamation rule: no running job and no job finished
// within the TTL, and the pod itself is at least TTL old.
func isIdle(createdAt, now int64, ttl time.Duration, jobs []Job) bool {
	if createdAt == 0 || now-createdAt < ttl.Milliseconds() {
		return false
	}
	for _, j := range jobs {
		if j.State == "running" {
			return false
		}
		if j.FinishedAt > 0 && now-j.FinishedAt < ttl.Milliseconds() {
			return false
		}
	}
	return true
}
