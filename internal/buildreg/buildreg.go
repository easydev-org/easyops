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
	"strings"
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

// DefaultMaxRecords caps the registry (metadata only — logs are capped
// separately, see LogCapBytes / LogBudgetBytes).
const DefaultMaxRecords = 1000

// LogCapBytes bounds ONE build's stored log. A build log is unbounded (a large
// Go build can emit megabytes), so it is truncated here and the record marks
// itself truncated — the caller is never silently given a partial log.
//
// The HEAD is kept, not the tail: a truncated log is read top-down, and the
// interesting part of a failure (the exit status) is usually near the end, but
// pagination is only coherent if offset 0 is the real offset 0.
const LogCapBytes = 1 << 20 // 1 MiB

// LogBudgetBytes bounds the TOTAL log memory across all records.
//
// This is the number that actually protects the process. A per-record cap alone
// would allow MaxRecords*LogCapBytes = 1000 * 1 MiB = 1 GiB, which would OOM a
// pod that has no memory limit set. The budget is enforced by dropping the logs
// of the OLDEST FINISHED records first (their metadata stays, so List/Get still
// report what happened). A running build's log is never dropped.
const LogBudgetBytes = 64 << 20 // 64 MiB

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
	Log string
	// LogBytes is the FULL size of the build's output, even when Log was
	// truncated to LogCapBytes.
	LogBytes int64
	// LogTruncated reports that Log holds only the first part of the output.
	LogTruncated bool
	// LogDropped reports that the log was RELEASED to stay within the global
	// budget (the build finished long ago). Metadata is still available.
	LogDropped bool
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
	logCap  int
	budget  int
	logUsed int
}

// New builds a registry holding at most max records (<=0 uses the default).
func New(max int) *Registry {
	if max <= 0 {
		max = DefaultMaxRecords
	}
	return &Registry{byID: map[string]*Record{}, max: max, logCap: LogCapBytes, budget: LogBudgetBytes}
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

// AppendLog adds output, keeping at most logCap bytes per record and enforcing
// the global log budget.
func (g *Registry) AppendLog(id, chunk string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.byID[id]
	if !ok {
		return
	}
	rec.LogBytes += int64(len(chunk))
	// Once a record is truncated we stop buffering entirely: the head is already
	// full and appending would only re-truncate. LogBytes keeps counting so the
	// caller can see how much output there really was.
	if rec.LogTruncated {
		return
	}
	room := g.logCap - len(rec.Log)
	if room <= 0 {
		rec.LogTruncated = true
		return
	}
	if len(chunk) > room {
		rec.Log += chunk[:room]
		rec.LogTruncated = true
		g.logUsed += room
		g.enforceBudgetLocked()
		return
	}
	rec.Log += chunk
	g.logUsed += len(chunk)
	g.enforceBudgetLocked()
}

// enforceBudgetLocked drops the logs of the oldest FINISHED records until the
// total is within budget. Metadata survives, so List/Get still report what
// happened; only the log body is released.
func (g *Registry) enforceBudgetLocked() {
	if g.logUsed <= g.budget {
		return
	}
	for _, id := range g.order { // oldest first
		if g.logUsed <= g.budget {
			return
		}
		rec, ok := g.byID[id]
		if !ok || rec.Log == "" {
			continue
		}
		if rec.State == StateRunning {
			continue // never release a live build's log
		}
		g.logUsed -= len(rec.Log)
		rec.Log = ""
		rec.LogDropped = true
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

// LogPage is a window of a build's log, by LINE.
type LogPage struct {
	// Lines are the requested lines, WITHOUT line numbers (the caller formats
	// them: numbering is a presentation concern, and a client may want raw text).
	Lines []string
	// FirstLine is the 1-based number of Lines[0], so the caller can print
	// "12: ..." for a page that starts at line 12.
	FirstLine int
	// TotalLines is the number of lines the record holds.
	TotalLines int
	// Truncated reports that the BUILD's output exceeded the record's cap, so
	// there is more output than this record can ever show.
	Truncated bool
	// Dropped reports that the log was released to stay within the global
	// budget (old finished build); there is nothing to read.
	Dropped bool
}

// DefaultLogLines / MaxLogLines bound a log page. The default keeps a tool
// response small; the max stops a caller from asking for a 1 MiB wall of text
// in one shot (it can still page through it).
const (
	DefaultLogLines = 200
	MaxLogLines     = 1000
)

// LogPage returns a window of a build's log by line number.
//
// offset is 0-based ("skip this many lines"); limit<=0 uses DefaultLogLines and
// is capped at MaxLogLines. Lines are returned WITHOUT numbers so a non-text
// consumer (a script) is not forced to strip a prefix; the page carries
// FirstLine so a human-facing renderer can number them.
func (g *Registry) LogPage(id string, offset, limit int) (LogPage, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rec, ok := g.byID[id]
	if !ok {
		return LogPage{}, false
	}
	page := LogPage{Truncated: rec.LogTruncated, Dropped: rec.LogDropped}
	if rec.Log == "" {
		return page, true
	}
	if limit <= 0 {
		limit = DefaultLogLines
	}
	if limit > MaxLogLines {
		limit = MaxLogLines
	}
	if offset < 0 {
		offset = 0
	}
	// Split without a trailing empty element for a log that ends in "\n".
	all := strings.Split(strings.TrimSuffix(rec.Log, "\n"), "\n")
	if len(all) == 1 && all[0] == "" {
		all = nil
	}
	page.TotalLines = len(all)
	if offset >= len(all) {
		return page, true
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	page.Lines = all[offset:end]
	page.FirstLine = offset + 1
	return page, true
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
			// Reclaim the log's share of the budget, or the accounting leaks and
			// the budget eventually rejects every new log.
			g.logUsed -= len(rec.Log)
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
