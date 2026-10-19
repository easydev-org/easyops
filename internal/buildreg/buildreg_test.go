package buildreg

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestBuildLifecycle pins the state machine: start -> done, with the image ref
// only on success and a terminal state in both cases.
func TestBuildLifecycle(t *testing.T) {
	g := New(10)
	ctx := context.Background()

	id, _ := g.Start(ctx, "acme", "app", "v1", true)
	rec, ok := g.Get(id)
	if !ok || rec.State != StateRunning {
		t.Fatalf("after Start: %+v ok=%v", rec, ok)
	}
	if rec.FinishedAt != 0 {
		t.Errorf("a running build must not have a finish time")
	}

	g.Finish(id, "registry/app:v1", nil)
	rec, _ = g.Get(id)
	if rec.State != StateDone || rec.Image != "registry/app:v1" {
		t.Errorf("after success: %+v", rec)
	}
	if rec.FinishedAt == 0 {
		t.Error("a finished build must have a finish time")
	}
}

// TestFailureKeepsError: a failed build records the error and no image.
func TestFailureKeepsError(t *testing.T) {
	g := New(10)
	id, _ := g.Start(context.Background(), "acme", "app", "v1", false)
	g.Finish(id, "", errString("boom"))

	rec, _ := g.Get(id)
	if rec.State != StateFailed || rec.Error == "" {
		t.Errorf("failure not recorded: %+v", rec)
	}
	if rec.Image != "" {
		t.Errorf("a failed build must not carry an image ref: %q", rec.Image)
	}
}

// TestCancel: cancelling flips the state, cancels the context the build runs
// under, and a later Finish must NOT overwrite the canceled state.
func TestCancel(t *testing.T) {
	g := New(10)
	id, bctx := g.Start(context.Background(), "acme", "app", "v1", false)

	done := make(chan struct{})
	go func() {
		<-bctx.Done()
		close(done)
	}()
	// Give the watcher a chance to reach its receive before cancelling, so the
	// assertion below tests the cancellation itself rather than scheduling.
	runtime.Gosched()

	rec, ok := g.Cancel(id)
	if !ok || rec.State != StateCanceled {
		t.Fatalf("cancel: %+v ok=%v", rec, ok)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Cancel must cancel the build's context")
	}

	// The build goroutine unwinds and calls Finish with ctx.Err(); that must not
	// resurrect it as "failed".
	g.Finish(id, "", errString("context canceled"))
	rec, _ = g.Get(id)
	if rec.State != StateCanceled {
		t.Errorf("Finish overwrote canceled with %q", rec.State)
	}
}

// TestCancelUnknown: cancelling an unknown id reports not-found.
func TestCancelUnknown(t *testing.T) {
	g := New(10)
	if _, ok := g.Cancel("nope"); ok {
		t.Error("cancelling an unknown build must report false")
	}
}

// TestLogTailIsBounded: the log keeps only the LAST LogTailBytes, because a
// failure explains itself at the end and a build log is unbounded.
func TestLogTailIsBounded(t *testing.T) {
	g := New(10)
	id, _ := g.Start(context.Background(), "acme", "app", "v1", false)
	big := make([]byte, LogTailBytes*3)
	for i := range big {
		big[i] = 'x'
	}
	g.AppendLog(id, string(big))
	g.AppendLog(id, "TAIL-MARKER")

	rec, _ := g.Get(id)
	if len(rec.Log) > LogTailBytes {
		t.Errorf("log = %d bytes, want <= %d", len(rec.Log), LogTailBytes)
	}
	if !hasSuffix(rec.Log, "TAIL-MARKER") {
		t.Error("the tail must survive truncation")
	}
}

// TestEvictionDropsOldestFinished: beyond the cap the oldest FINISHED records
// go; the registry never grows without bound.
func TestEvictionDropsOldestFinished(t *testing.T) {
	g := New(3)
	var ids []string
	for i := 0; i < 5; i++ {
		id, _ := g.Start(context.Background(), "acme", "app", "v", false)
		g.Finish(id, "ref", nil)
		ids = append(ids, id)
	}
	if got := g.Len(); got != 3 {
		t.Fatalf("len = %d, want 3", got)
	}
	if _, ok := g.Get(ids[0]); ok {
		t.Error("the oldest record must be evicted")
	}
	if _, ok := g.Get(ids[4]); !ok {
		t.Error("the newest record must survive")
	}
}

// TestRunningIsNeverEvicted: evicting a live build would orphan its goroutine,
// so the registry may temporarily overshoot instead.
func TestRunningIsNeverEvicted(t *testing.T) {
	g := New(2)
	run1, _ := g.Start(context.Background(), "acme", "app", "v", false)
	run2, _ := g.Start(context.Background(), "acme", "app", "v", false)
	run3, _ := g.Start(context.Background(), "acme", "app", "v", false)

	for _, id := range []string{run1, run2, run3} {
		if _, ok := g.Get(id); !ok {
			t.Errorf("running build %s was evicted", id)
		}
	}
	if g.Len() < 3 {
		t.Errorf("len = %d, want at least 3 (overshoot is allowed)", g.Len())
	}
}

// TestListNewestFirstAndOwnerFilter: List is newest-first and can be scoped.
func TestListNewestFirstAndOwnerFilter(t *testing.T) {
	g := New(10)
	a1, _ := g.Start(context.Background(), "alice", "app", "v1", false)
	g.Finish(a1, "r1", nil)
	b1, _ := g.Start(context.Background(), "bob", "app", "v1", false)
	g.Finish(b1, "r2", nil)
	a2, _ := g.Start(context.Background(), "alice", "app", "v2", false)
	g.Finish(a2, "r3", nil)

	all := g.List("", 0)
	if len(all) != 3 {
		t.Fatalf("all = %d", len(all))
	}
	if all[0].ID != a2 {
		t.Errorf("newest first: got %s, want %s", all[0].ID, a2)
	}
	mine := g.List("alice", 0)
	if len(mine) != 2 {
		t.Errorf("alice = %d, want 2", len(mine))
	}
	limited := g.List("", 2)
	if len(limited) != 2 {
		t.Errorf("limit 2 returned %d", len(limited))
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
