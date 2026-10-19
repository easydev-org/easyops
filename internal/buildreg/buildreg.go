// Package buildreg is an in-memory registry of image builds.
//
// WHY it exists: BuildImage/BuildImageStream are single blocking RPCs, so a
// build has no identity and no memory — once the call returns, nothing is left
// to query. That made "what has been built?" and "did the last build succeed?"
// unanswerable. This registry gives each build an id, a state machine and a
// bounded log tail, so ListBuilds/GetBuild can answer after the fact.
//
// DURABILITY: records live in this process's memory, deliberately.
//
//   - a restart clears them;
//   - a multi-replica deployment has one independent set per replica.
//
// That is the right trade for a RUNTIME VIEW of recent activity, and it keeps
// easyops stateless (no DB, no PVC). Every record reports `durability=memory`
// so a client cannot mistake it for an audit log.
//
// The registry keeps the newest MaxRecords entries and evicts the oldest. A
// finished build older than that is dropped; a RUNNING build is never evicted
// (it would orphan its goroutine).
package buildreg

import (
	"context"
	"sync"
	"time"
)

// State is a build's lifecycle state.
type State string

const (
	StateRunning  State = "running"
	StateDone     State = "done"
	StateFailed   State = "failed"
	StateCanceled State = "canceled"
)

// Terminal reports whether s is a final state.
func (s State) Terminal() bool {
	switch s {
	case StateDone, StateFailed, StateCanceled:
		return true
	}
	return false
}

// DefaultMaxRecords caps the registry. A build record holds a bounded log
// tail, so this is the memory ceiling: MaxRecords * LogTailBytes.
//
// 1000 records x 4 KiB ~= 4 MiB worst case, which is a sane bound for a
// PROCESS-LOCAL view of recent builds. Raise it only with that arithmetic in
// mind: the log tail is what dominates, not the record fields.
const DefaultMaxRecords = 1000

// LogTailBytes bounds the log kept per record. A build log is unbounded (a
// large Go build can emit megabytes); keeping the TAIL is what matters because
// a failure explains itself at the end.
const LogTailBytes = 4096

// Record is one build.
type Record struct {
	ID    string
	Owner string
	Repo  string
	Tag   string
	Image string // resolved ref, set on success
	State State
	Error string
	// Log holds the most recent LogTailBytes of build output.
	Log        string
	StartedAt  int64 // unix millis
	FinishedAt int64 // unix millis (0 while running)
	CacheUsed  bool
	// Seq is a monotonically increasing creation counter; List returns newest
	// first by it (StartedAt can collide at millisecond resolution).
	Seq int64

	cancel context.CancelFunc
}

// Snapshot is a copy safe to hand to a caller (no cancel func, no mutex).
func (r Record) Snapshot() Record {
	r.cancel = nil
	return r
}

// Registry is a bounded, concurrency-safe build registry.
type Registry struct {
	mu      sync.RWMutex
	seq     int64
	order   []string // ids, oldest first (eviction order)
	byID    map[string]*Record
	max     int
	tailCap int
}

// New builds a registry holding at most max records (<=0 uses the default).
func New(max int) *Registry {
	if max <= 0 {
		max = DefaultMaxRecords
	}
	return &Registry{byID: map[string]*Record{}, max: max, tailCap: LogTailBytes}
}

// Start registers a RUNNING build and returns its id plus a context that
// Cancel cancels. The caller runs the build under that context and reports the
// outcome with Finish.
func (g *Registry) Start(parent context.Context, owner, repo, tag string, cacheUsed bool) (string, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	id := newID(g.seq)
	rec := &Record{
		ID: id, Owner: owner, Repo: repo, Tag: tag,
		State: StateRunning, StartedAt: time.Now().UnixMilli(),
		CacheUsed: cacheUsed, Seq: g.seq, cancel: cancel,
	}
	g.byID[id] = rec
	g.order = append(g.order, id)
	g.evictLocked()
	return id, ctx
}

// Finish records the terminal outcome. It is a no-op if the build was already
// finished or evicted (Cancel raced with completion).
func (g *Registry) Finish(id string, imageRef string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.byID[id]
	if !ok {
		return // evicted
	}
	if rec.State.Terminal() {
		return // already canceled/finished
	}
	rec.FinishedAt = time.Now().UnixMilli()
	if err != nil {
		if rec.State == StateCanceled {
			// keep canceled; the error is the ctx cancellation
			return
		}
		rec.State = StateFailed
		rec.Error = err.Error()
		return
	}
	rec.State = StateDone
	rec.Image = imageRef
}

// AppendLog adds output, keeping only the last tailCap bytes.
func (g *Registry) AppendLog(id, chunk string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.byID[id]
	if !ok {
		return
	}
	rec.Log += chunk
	if len(rec.Log) > g.tailCap {
		rec.Log = rec.Log[len(rec.Log)-g.tailCap:]
	}
}

// Get returns one build.
func (g *Registry) Get(id string) (Record, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rec, ok := g.byID[id]
	if !ok {
		return Record{}, false
	}
	return rec.Snapshot(), true
}

// List returns builds newest-first, filtered by owner ("" = all). limit<=0
// returns everything the registry holds.
func (g *Registry) List(owner string, limit int) []Record {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Record, 0, len(g.order))
	for i := len(g.order) - 1; i >= 0; i-- {
		rec, ok := g.byID[g.order[i]]
		if !ok {
			continue
		}
		if owner != "" && rec.Owner != owner {
			continue
		}
		out = append(out, rec.Snapshot())
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Cancel requests cancellation. It reports false when the build is unknown, and
// leaves a terminal build untouched.
func (g *Registry) Cancel(id string) (Record, bool) {
	g.mu.Lock()
	rec, ok := g.byID[id]
	if !ok {
		g.mu.Unlock()
		return Record{}, false
	}
	if rec.State.Terminal() {
		snap := rec.Snapshot()
		g.mu.Unlock()
		return snap, true
	}
	rec.State = StateCanceled
	rec.FinishedAt = time.Now().UnixMilli()
	cancel := rec.cancel
	snap := rec.Snapshot()
	g.mu.Unlock()
	// Cancel AFTER releasing the lock: the build goroutine may immediately
	// unwind and call Finish, which takes the same lock. Cancelling under the
	// lock would deadlock against that.
	if cancel != nil {
		cancel()
	}
	return snap, true
}

// Len reports how many records are held (for tests/metrics).
func (g *Registry) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.byID)
}

// evictLocked drops the oldest records until the registry is within max.
// A RUNNING build is skipped over (its goroutine still holds the id): evicting
// it would leave the goroutine writing into a record nobody can see. Skipping
// is safe because running builds are bounded by concurrency, not by history.
func (g *Registry) evictLocked() {
	for len(g.order) > g.max {
		dropped := false
		for i := 0; i < len(g.order); i++ {
			rec, ok := g.byID[g.order[i]]
			if !ok {
				g.order = append(g.order[:i], g.order[i+1:]...)
				dropped = true
				break
			}
			if rec.State == StateRunning {
				continue // never drop a live build
			}
			delete(g.byID, g.order[i])
			g.order = append(g.order[:i], g.order[i+1:]...)
			dropped = true
			break
		}
		if !dropped {
			// Everything left is running: allow a temporary overshoot rather
			// than orphan a goroutine.
			return
		}
	}
}

// newID renders a sortable id: b<seq>. The seq alone is enough because it is
// unique within one process, and the registry is per-process by design.
func newID(seq int64) string {
	return "b" + itoa(seq)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
