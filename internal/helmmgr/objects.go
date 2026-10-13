package helmmgr

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ObjectStatus is the live status of one applied release object (best-effort).
type ObjectStatus struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	// Status is a coarse phase: Ready | Progressing | Failed | Complete |
	// NotFound | <Pod phase>. Ready is the boolean form.
	Status string
	Ready  bool
	// Message is the first unhealthy container's waiting/terminated reason
	// (e.g. CrashLoopBackOff), or the lookup error. Empty when healthy.
	Message string
	// Pod-only / workload diagnostics.
	Phase           string
	Restarts        int32
	ReadyReplicas   int32
	DesiredReplicas int32
}

// currentObjects returns the applied objects of a release's current revision.
func (c *Client) currentObjects(ctx context.Context, release string) ([]Object, error) {
	rel, ok, err := c.Get(ctx, release)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("release %q not found", release)
	}
	if len(rel.History) == 0 {
		return nil, nil
	}
	return rel.History[len(rel.History)-1].Objects, nil
}

// HasObject reports whether (kind, name) is one of the release's CURRENT
// objects. It is the ownership gate for per-object log access.
func (c *Client) HasObject(ctx context.Context, release, kind, name string) (bool, error) {
	objs, err := c.currentObjects(ctx, release)
	if err != nil {
		return false, err
	}
	for _, o := range objs {
		if strings.EqualFold(o.Kind, kind) && o.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ObjectStatuses returns the live status of every object in the release's
// current revision. A single object's lookup failure is reported IN that
// object's Message (never aborts the whole list).
func (c *Client) ObjectStatuses(ctx context.Context, release string) ([]ObjectStatus, error) {
	objs, err := c.currentObjects(ctx, release)
	if err != nil {
		return nil, err
	}
	out := make([]ObjectStatus, 0, len(objs))
	for _, o := range objs {
		out = append(out, c.objectStatus(ctx, o))
	}
	return out, nil
}

func (c *Client) objectStatus(ctx context.Context, o Object) ObjectStatus {
	st := ObjectStatus{APIVersion: o.APIVersion, Kind: o.Kind, Name: o.Name, Namespace: c.namespace}
	gv, err := schema.ParseGroupVersion(o.APIVersion)
	if err != nil {
		st.Message = "bad apiVersion: " + err.Error()
		return st
	}
	gvk := gv.WithKind(o.Kind)
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		st.Message = err.Error()
		return st
	}
	u, err := c.dyn.Resource(mapping.Resource).Namespace(c.namespace).Get(ctx, o.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			st.Status = "NotFound"
			return st
		}
		st.Message = err.Error()
		return st
	}
	c.fillStatus(ctx, &st, u)
	return st
}

// fillStatus derives Ready/Status/Message (+ workload/pod diagnostics) from an
// object's live unstructured form.
func (c *Client) fillStatus(ctx context.Context, st *ObjectStatus, u *unstructured.Unstructured) {
	// Workloads: spec.replicas + status.readyReplicas.
	if reps, found, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); found {
		ready, _, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas")
		st.DesiredReplicas = int32(reps)
		st.ReadyReplicas = int32(ready)
		st.Ready = reps == 0 || ready >= reps
		st.Status = readyPhase(st.Ready)
		// Surface why a workload is not ready: the first bad pod's reason.
		if !st.Ready {
			st.Restarts, st.Message = c.workloadPodDiagnostics(ctx, u)
		}
		return
	}
	switch u.GetKind() {
	case "Pod":
		phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
		st.Status, st.Phase = phase, phase
		st.Ready = podReady(u)
		st.Restarts, st.Message = podDiagnostics(u)
	case "Job":
		succ, _, _ := unstructured.NestedInt64(u.Object, "status", "succeeded")
		failed, _, _ := unstructured.NestedInt64(u.Object, "status", "failed")
		switch {
		case succ > 0:
			st.Status, st.Ready = "Complete", true
		case failed > 0:
			st.Status = "Failed"
		default:
			st.Status = "Running"
		}
	default:
		// Service/ConfigMap/Secret/PVC/...: present == ready.
		st.Status, st.Ready = "Ready", true
	}
}

// workloadPodDiagnostics lists the pods a workload selects and returns the total
// restarts + the first unhealthy reason (best-effort; empty on any error).
func (c *Client) workloadPodDiagnostics(ctx context.Context, u *unstructured.Unstructured) (int32, string) {
	sel, found, _ := unstructured.NestedStringMap(u.Object, "spec", "selector", "matchLabels")
	if !found || len(sel) == 0 {
		return 0, ""
	}
	parts := make([]string, 0, len(sel))
	for k, v := range sel {
		parts = append(parts, k+"="+v)
	}
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: strings.Join(parts, ","),
	})
	if err != nil {
		return 0, ""
	}
	var restarts int32
	message := ""
	for i := range pods.Items {
		r, m := corePodDiagnostics(&pods.Items[i])
		restarts += r
		if message == "" && m != "" {
			message = m
		}
	}
	return restarts, message
}

// corePodDiagnostics is podDiagnostics for a typed Pod (workload pod listing).
func corePodDiagnostics(p *corev1.Pod) (int32, string) {
	var restarts int32
	message := ""
	for _, cs := range p.Status.ContainerStatuses {
		restarts += cs.RestartCount
		if message != "" {
			continue
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			message = cs.State.Waiting.Reason
		} else if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" &&
			cs.State.Terminated.Reason != "Completed" {
			message = cs.State.Terminated.Reason
		}
	}
	return restarts, message
}

// podReady reports whether a Pod's Ready condition is True.
func podReady(u *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "Ready" {
			return m["status"] == "True"
		}
	}
	return false
}

// podDiagnostics returns total restarts + the first waiting/terminated reason of
// a Pod (unstructured form).
func podDiagnostics(u *unstructured.Unstructured) (int32, string) {
	statuses, _, _ := unstructured.NestedSlice(u.Object, "status", "containerStatuses")
	var restarts int32
	message := ""
	for _, cs := range statuses {
		m, ok := cs.(map[string]any)
		if !ok {
			continue
		}
		if rc, ok := m["restartCount"].(int64); ok {
			restarts += int32(rc)
		}
		if message == "" {
			if w, ok := m["state"].(map[string]any); ok {
				if wm, ok := w["waiting"].(map[string]any); ok {
					if r, _ := wm["reason"].(string); r != "" {
						message = r
					}
				} else if tm, ok := w["terminated"].(map[string]any); ok {
					if r, _ := tm["reason"].(string); r != "" && r != "Completed" {
						message = r
					}
				}
			}
		}
	}
	return restarts, message
}

func readyPhase(ready bool) string {
	if ready {
		return "Ready"
	}
	return "Progressing"
}

// LogOptions selects which part of a Pod's container log to read.
type LogOptions struct {
	TailLines int64
	Previous  bool
	Follow    bool
	// Container names the container ("" = the pod's first container).
	Container string
}

// ObjectLogs reads up to opts.TailLines lines of a Pod object's container log.
// Only Pod objects have logs; anything else yields an error.
func (c *Client) ObjectLogs(ctx context.Context, kind, name string, opts LogOptions) ([]string, error) {
	if !strings.EqualFold(kind, "Pod") {
		return nil, fmt.Errorf("logs are only available for Pod objects, not %q", kind)
	}
	container := opts.Container
	if container == "" {
		pod, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if len(pod.Spec.Containers) == 0 {
			return nil, fmt.Errorf("pod %q has no containers", name)
		}
		container = pod.Spec.Containers[0].Name
	}
	o := &corev1.PodLogOptions{Container: container, Follow: opts.Follow}
	if opts.TailLines > 0 {
		n := opts.TailLines
		o.TailLines = &n
	}
	if opts.Previous {
		o.Previous = true
	}
	rc, err := c.cs.CoreV1().Pods(c.namespace).GetLogs(name, o).Stream(ctx)
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
