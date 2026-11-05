package sandboxmgr

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// WaitReady polls until the sandbox's worker is READY, or the timeout elapses.
//
// Readiness is the kubelet's verdict (the pod's containers report Ready), which
// is why the sandbox container carries a TCP readiness probe on WorkerPort: that
// probe only passes once the worker has actually bound its port.
//
// It deliberately does NOT dial the pod IP or `<svc>.<ns>.svc.cluster.local`.
// Neither is reachable from an easyops that runs OUTSIDE the cluster it manages —
// which k8sconfig explicitly supports (an injected kubeconfig may point at
// another cluster or another namespace's identity). The old dial-based check made
// CreateSandbox fail with "not healthy within 2m" for a pod that was 1/1 Running
// and a Service that was perfectly correct, because the check could never
// succeed from the server's own process. Connectivity is what CALLERS verify;
// this is a lifecycle check.
func (c *Client) WaitReady(ctx context.Context, name string, timeout time.Duration) error {
	res := resourceName(name)
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, perr := c.depPod(ctx, res)
		if perr != nil {
			pod = nil
		}
		if pod != nil {
			// A container that cannot start (ImagePullBackOff, ErrImagePull,
			// CreateContainerConfigError, CrashLoopBackOff) never becomes ready.
			// Surface the reason immediately instead of waiting out the whole
			// timeout: the caller's error is what an operator reads.
			//
			// Only a TERMINATED container with RestartPolicy=Never is `Failed`;
			// a sandbox runs RestartPolicy=Always, so the waiting reason on the
			// container STATUS is the signal (checking pod.Phase alone missed it
			// and made WaitReady burn its entire timeout).
			if reason := firstWaitingReason(pod); reason != "" {
				return fmt.Errorf("sandbox %s cannot start: %s", name, reason)
			}
			if pod.Status.Phase == "Failed" {
				return fmt.Errorf("sandbox %s exited: %s", name, firstFailureReason(pod))
			}
			if pod.Status.Phase == "Running" && workerReady(pod) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("sandbox %s not healthy within %s", name, timeout)
}

// workerReady reports whether the worker container has passed its readiness
// probe (all containers ready). A running pod whose container has not reported
// Ready yet is still starting (the httpd is bound but has not answered).
func workerReady(p *corev1.Pod) bool {
	if len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

// firstFailureReason returns the first container's failure reason+message (e.g.
// `OOMKilled`), or a generic message when none is reported.
func firstFailureReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			msg := cs.State.Terminated.Reason
			if cs.State.Terminated.Message != "" {
				msg += ": " + cs.State.Terminated.Message
			}
			return msg
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
	}
	return "pod failed"
}

// firstWaitingReason returns a container's CURRENT waiting reason when it is a
// failure the kubelet will not recover from on its own (an image that cannot be
// pulled, a config error, a crash loop), prefixed with the message.
//
// A normal "ContainerCreating" is NOT a failure and is ignored: every pod
// waits there for a moment.
func firstWaitingReason(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		switch w.Reason {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
			"CreateContainerConfigError", "CreateContainerError",
			"CrashLoopBackOff", "RunContainerError":
			if w.Message != "" {
				return w.Reason + ": " + w.Message
			}
			return w.Reason
		}
	}
	return ""
}
