package sandboxmgr

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// toSandbox maps a sandbox Pod to its live view, including best-effort pod
// diagnostics (restarts + the first unhealthy container's reason/exit code/
// lastState) so a Failed sandbox explains itself.
func toSandbox(c *Client, p *corev1.Pod) Sandbox {
	ready := false
	var restarts int32
	message := ""
	var exitCode int32
	var termReason, lastReason, containerName string
	var startedAt, finishedAt int64
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready = true
		}
		restarts += cs.RestartCount
		// First failure reason wins (waiting => crash-loop/creating; terminated
		// => OOMKilled / Error after a restart policy of Never).
		if message == "" && cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			message = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				message += ": " + cs.State.Waiting.Message
			}
			containerName = cs.Name
		}
		if message == "" && cs.State.Terminated != nil && cs.State.Terminated.Reason != "" &&
			cs.State.Terminated.Reason != "Completed" {
			message = cs.State.Terminated.Reason
			if cs.State.Terminated.Message != "" {
				message += ": " + cs.State.Terminated.Message
			}
			containerName = cs.Name
			exitCode = cs.State.Terminated.ExitCode
			termReason = cs.State.Terminated.Reason
			if !cs.State.Terminated.StartedAt.IsZero() {
				startedAt = cs.State.Terminated.StartedAt.UnixMilli()
			}
			if !cs.State.Terminated.FinishedAt.IsZero() {
				finishedAt = cs.State.Terminated.FinishedAt.UnixMilli()
			}
		}
		// lastState = what the container did before its current state.
		if lastReason == "" && cs.LastTerminationState.Terminated != nil {
			if r := cs.LastTerminationState.Terminated.Reason; r != "" && r != "Completed" {
				lastReason = r
			}
		}
		if lastReason == "" && cs.LastTerminationState.Waiting != nil {
			lastReason = cs.LastTerminationState.Waiting.Reason
		}
	}
	phase := string(p.Status.Phase)
	if phase == "" {
		phase = "Unknown"
	}
	created := p.CreationTimestamp.UnixMilli()
	if created == 0 {
		created = time.Now().UnixMilli()
	}
	return Sandbox{
		Name:             p.Labels[LabelName],
		Image:            p.Annotations[AnnoImage],
		Phase:            phase,
		Ready:            ready,
		URL:              c.ServiceDNS(p.Name),
		Creator:          p.Annotations[AnnoCreator],
		Session:          p.Annotations[AnnoSession],
		CreatedAt:        created,
		Restarts:         restarts,
		Message:          message,
		ExitCode:         exitCode,
		TerminatedReason: termReason,
		LastStateReason:  lastReason,
		Container:        containerName,
		StartedAt:        startedAt,
		FinishedAt:       finishedAt,
	}
}
