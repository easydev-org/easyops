// Package obs is the platform's observability bootstrap. It is deliberately
// ZERO-DEPENDENCY (stdlib only): every service can adopt it without pulling a
// metrics stack, and it emits Prometheus text exposition format.
//
// It provides request counters, latency histograms (per RPC + status code) and
// a /metrics + /healthz server.
package obs

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// rpcBuckets are the default latency buckets (seconds).
var rpcBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Registry holds counters and histograms. A nil *Registry is a valid no-op.
type Registry struct {
	mu       sync.Mutex
	counters map[string]float64
	hists    map[string]*histogram
}

type histogram struct {
	buckets []float64
	counts  []uint64
	sum     float64
	count   uint64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{counters: map[string]float64{}, hists: map[string]*histogram{}}
}

// Default is the process-wide registry.
var Default = NewRegistry()

func key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range ks {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=\"%s\"", k, escapeLabel(labels[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabel escapes a label value for the Prometheus text format. Only the
// three escapes the exposition format defines are emitted; a raw backslash,
// double quote or newline in a label value would otherwise produce a line the
// scraper rejects.
func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// Inc adds 1 to the counter name{labels}.
func (r *Registry) Inc(name string, labels map[string]string) { r.Add(name, labels, 1) }

// Add adds v to the counter name{labels}.
func (r *Registry) Add(name string, labels map[string]string, v float64) {
	if r == nil {
		return
	}
	k := key(name, labels)
	r.mu.Lock()
	r.counters[k] = r.counters[k] + v
	r.mu.Unlock()
}

// Observe records v (seconds) in the histogram name{labels}.
func (r *Registry) Observe(name string, labels map[string]string, v float64) {
	if r == nil {
		return
	}
	k := key(name, labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.hists[k]
	if h == nil {
		h = &histogram{buckets: rpcBuckets, counts: make([]uint64, len(rpcBuckets))}
		r.hists[k] = h
	}
	h.count++
	h.sum += v
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
}

// ObserveRPC records one RPC: a counter and a latency observation.
func (r *Registry) ObserveRPC(proc, code string, d time.Duration) {
	r.Inc("rpc_requests_total", map[string]string{"proc": proc, "code": code})
	r.Observe("rpc_duration_seconds", map[string]string{"proc": proc}, d.Seconds())
}

// ObserveRPC records one RPC on the Default registry.
func ObserveRPC(proc, code string, d time.Duration) { Default.ObserveRPC(proc, code, d) }

// Render writes the registry in Prometheus text exposition format. Every metric
// family is emitted with a SINGLE # TYPE line: the text format forbids a second
// TYPE line — or a TYPE after samples — for the same name, so a family with
// more than one label set must not repeat it.
func (r *Registry) Render(w io.Writer) {
	if r == nil {
		return
	}
	// Render into a buffer while holding the lock, then write the bytes out:
	// a slow scrape must not block Add/Observe.
	var b bytes.Buffer
	r.mu.Lock()
	r.renderLocked(&b)
	r.mu.Unlock()
	_, _ = w.Write(b.Bytes())
}

func (r *Registry) renderLocked(w io.Writer) {
	cks := make([]string, 0, len(r.counters))
	for k := range r.counters {
		cks = append(cks, k)
	}
	sort.Strings(cks)
	for i, k := range cks {
		// Keys are sorted, so equal family names are adjacent; the TYPE line is
		// emitted only when the family changes.
		if i == 0 || familyName(cks[i-1]) != familyName(k) {
			fmt.Fprintf(w, "# TYPE %s counter\n", familyName(k))
		}
		fmt.Fprintf(w, "%s %g\n", k, r.counters[k])
	}
	hks := make([]string, 0, len(r.hists))
	for k := range r.hists {
		hks = append(hks, k)
	}
	sort.Strings(hks)
	for i, k := range hks {
		h := r.hists[k]
		name, labels := splitKey(k)
		if i == 0 || familyName(hks[i-1]) != name {
			fmt.Fprintf(w, "# TYPE %s histogram\n", name)
		}
		for bi, b := range h.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", name, withLE(labels, fmt.Sprintf("%g", b)), h.counts[bi])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", name, withLE(labels, "+Inf"), h.count)
		fmt.Fprintf(w, "%s_sum%s %g\n", name, labels, h.sum)
		fmt.Fprintf(w, "%s_count%s %d\n", name, labels, h.count)
	}
}

// familyName returns the metric name of a stored key ("name" or "name{...}").
func familyName(k string) string {
	if i := strings.IndexByte(k, '{'); i >= 0 {
		return k[:i]
	}
	return k
}

func splitKey(k string) (name, labels string) {
	if i := strings.IndexByte(k, '{'); i >= 0 {
		return k[:i], k[i:]
	}
	return k, ""
}

func withLE(labels, le string) string {
	if labels == "" {
		return fmt.Sprintf("{le=%q}", le)
	}
	return labels[:len(labels)-1] + fmt.Sprintf(",le=%q}", le)
}

// Handler serves /metrics (Prometheus) and /healthz (200 OK).
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.Render(w)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Serve listens on addr and serves the registry. It blocks; run it in a
// goroutine.
func (r *Registry) Serve(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return http.Serve(ln, r.Handler())
}
