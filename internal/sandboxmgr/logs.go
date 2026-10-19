package sandboxmgr

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkerContainer is the single container every sandbox runs (see Create).
const WorkerContainer = "worker"

// LogOptions selects which part of a sandbox container's log to read.
type LogOptions struct {
	TailLines int64
	Previous  bool
	Follow    bool
}

// podLogOptions maps LogOptions to the k8s pod-log options for the worker
// container.
func podLogOptions(opts LogOptions) *corev1.PodLogOptions {
	o := &corev1.PodLogOptions{Container: WorkerContainer, Follow: opts.Follow}
	if opts.TailLines > 0 {
		n := opts.TailLines
		o.TailLines = &n
	}
	if opts.Previous {
		o.Previous = true
	}
	return o
}

// Logs reads up to opts.TailLines lines of a sandbox container's current (or
// previous) log. A missing pod yields a NotFound error (mapped by the caller).
func (c *Client) Logs(ctx context.Context, name string, opts LogOptions) ([]string, error) {
	o := podLogOptions(opts)
	o.Follow = false
	pod, err := c.depPod(ctx, resourceName(name))
	if err != nil {
		return nil, err
	}
	if pod == nil {
		return nil, fmt.Errorf("sandbox %s: no pod yet", name)
	}
	req := c.cs.CoreV1().Pods(c.namespace).GetLogs(pod.Name, o)
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var lines []string
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	return lines, sc.Err()
}

// Follow streams a sandbox container's log until ctx is cancelled or the
// container exits. Lines are delivered on the first channel; a terminal error
// (if any) on the second.
func (c *Client) Follow(ctx context.Context, name string, opts LogOptions) (<-chan string, <-chan error) {
	lines := make(chan string, 256)
	errc := make(chan error, 1)
	go func() {
		defer close(lines)
		defer close(errc)
		o := podLogOptions(opts)
		o.Follow = true
		pod, perr := c.depPod(ctx, resourceName(name))
		if perr != nil || pod == nil {
			if perr != nil {
				errc <- perr
			} else {
				errc <- fmt.Errorf("sandbox %s: no pod yet", name)
			}
			return
		}
		req := c.cs.CoreV1().Pods(c.namespace).GetLogs(pod.Name, o)
		rc, err := req.Stream(ctx)
		if err != nil {
			errc <- err
			return
		}
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case <-ctx.Done():
				return
			case lines <- strings.TrimRight(sc.Text(), "\r"):
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			errc <- err
		}
	}()
	return lines, errc
}

// Events returns recent Kubernetes events for a sandbox's pod, newest-first and
// bounded to limit, each rendered as "reason: message". It is BEST-EFFORT: a
// caller without the `events` read permission gets an empty slice (not an
// error), so diagnostics still work without the extra RBAC.
func (c *Client) Events(ctx context.Context, name string, limit int) []string {
	if limit <= 0 {
		limit = 20
	}
	pod, perr := c.depPod(ctx, resourceName(name))
	if perr != nil || pod == nil {
		return nil
	}
	list, err := c.cs.CoreV1().Events(c.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + pod.Name,
	})
	if err != nil {
		return nil
	}
	items := list.Items
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].LastTimestamp.After(items[j].LastTimestamp.Time)
	})
	out := make([]string, 0, limit)
	for i := range items {
		if len(out) >= limit {
			break
		}
		e := items[i]
		line := e.Reason
		if e.Message != "" {
			if line != "" {
				line += ": "
			}
			line += e.Message
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
