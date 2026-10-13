package opssvc

import (
	"context"
	"fmt"
	"sort"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	opsv1connect "github.com/easy-platform/easyops/gen/easyops/v1/opsv1connect"
	"github.com/easy-platform/easyops/internal/servicesmgr"
)

// errNoServiceBackend is returned by every service/PVC RPC when the services
// backend is not configured.
func errNoServiceBackend() error {
	return connect.NewError(connect.CodeUnavailable, "service backend not configured")
}

// ---- services ----

// DeployService creates/updates a long-lived service owned by `owner`.
func (s *Service) DeployService(ctx context.Context, req *opsv1.DeployServiceRequest) (*opsv1.DeployServiceResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	m := req
	name := m.GetName()
	if name == "" {
		name = servicesmgr.ServiceName(m.GetSession())
	}
	ports, err := servicePorts(m.GetServices(), m.GetContainerPort())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	// B1: the manager's own conflict precheck only inspects the k8s SERVICES; a
	// bare same-named DEPLOYMENT (hand-created, or a Service was deleted) would
	// still be clobbered by the upsert. Refuse when an existing Deployment is not
	// writable by the caller (unowned included).
	if existing, gerr := s.services.Get(ctx, name); gerr == nil {
		if !writable(existing.Creator, m.GetOwner()) {
			return nil, connect.NewError(connect.CodePermissionDenied, "service not writable by this tenant")
		}
	}
	spec := servicesmgr.Spec{
		Namespace: m.GetNamespace(),
		Name:      name, Image: m.GetImage(), Command: m.GetCommand(),
		Env: m.GetEnv(), CPU: m.GetCpu(), Memory: m.GetMemory(),
		Replicas: m.GetReplicas(), ContainerPort: m.GetContainerPort(),
		ServicePort: m.GetServicePort(), Creator: m.GetOwner(), Session: m.GetSession(),
		Ports:          ports,
		Runtime:        s.runtime.Render(m.GetKvm(), int(m.GetGpuCount())),
		Volumes:        toVolumeMounts(m.GetVolumes()),
		Resources:      toResources(m.GetResources()),
		ReadinessProbe: toProbe(m.GetReadinessProbe()),
		LivenessProbe:  toProbe(m.GetLivenessProbe()),
		StartupProbe:   toProbe(m.GetStartupProbe()),
		Rollout:        toRollout(m.GetRollout()),
		EnvRefs:        toEnvRefs(m.GetEnvRefs()),
		EnvFrom:        toEnvFrom(m.GetEnvFrom()),
		ConfigMounts:   toConfigMounts(m.GetConfigMounts()),
		Sidecars:       toSidecars(m.GetSidecars()),
		NodeSelector:   m.GetNodeSelector(),
		Tolerations:    toTolerations(m.GetTolerations()),
	}
	if spec.Image == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "image is required")
	}
	// Enforce that the service image comes from artifact (any org is allowed; a
	// service runs the caller's own built image). See image_enforce.go.
	image, err := s.enforceServiceImage(spec.Image)
	if err != nil {
		return nil, err
	}
	spec.Image = image
	svc, err := s.services.Deploy(ctx, spec)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	// Bounded readiness wait: surface a deterministic start failure (bad
	// command, image pull error, unschedulable pod) right away instead of
	// returning a Pending shell indistinguishable from success. A genuine slow
	// start is NOT an error (the response view carries the observed phase).
	if err := s.awaitServiceReady(ctx, name); err != nil {
		return nil, err
	}
	if cur, gerr := s.services.Get(ctx, name); gerr == nil {
		svc = cur
	}
	return &opsv1.DeployServiceResponse{Service: toServiceInfo(svc)}, nil
}

// awaitServiceReady waits (bounded) for a just-deployed service to become ready.
// A deterministic failure returns FailedPrecondition with the pod reason; a
// timeout is non-fatal (the service is left running).
func (s *Service) awaitServiceReady(ctx context.Context, name string) error {
	res, err := s.services.WaitReady(ctx, name, serviceReadyTimeout)
	if err != nil {
		return mapK8sErr(err)
	}
	if res.Failed {
		return connect.Errorf(connect.CodeFailedPrecondition,
			"service %q failed to start: %s", name, res.Reason)
	}
	return nil
}

// ListServices lists the services visible to `owner`, newest-first.
func (s *Service) ListServices(ctx context.Context, req *opsv1.ListServicesRequest) (*opsv1.ListServicesResponse, error) {
	out, err := s.listServices(ctx, req.GetOwner())
	if err != nil {
		return nil, err
	}
	return &opsv1.ListServicesResponse{Services: out}, nil
}

// listServices returns the services visible to `owner`, newest-first. It returns
// an empty list (not an error) when the services backend is not configured.
func (s *Service) listServices(ctx context.Context, owner string) ([]*opsv1.ServiceInfo, error) {
	if s.services == nil {
		return nil, nil
	}
	svcs, err := s.services.List(ctx)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.ServiceInfo, 0, len(svcs))
	for _, svc := range svcs {
		if !owned(svc.Creator, owner) {
			continue
		}
		out = append(out, toServiceInfo(svc))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GetCreatedAt() > out[j].GetCreatedAt() })
	return out, nil
}

// GetService returns one service if visible to `owner`.
func (s *Service) GetService(ctx context.Context, req *opsv1.GetServiceRequest) (*opsv1.GetServiceResponse, error) {
	if s.services == nil {
		return &opsv1.GetServiceResponse{Found: false}, nil
	}
	svc, ok, err := s.ownedService(ctx, req.GetOwner(), req.GetName())
	if err != nil {
		return nil, err
	}
	if !ok {
		return &opsv1.GetServiceResponse{Found: false}, nil
	}
	return &opsv1.GetServiceResponse{Service: toServiceInfo(svc), Found: true}, nil
}

// DeleteService removes a service visible to `owner`.
func (s *Service) DeleteService(ctx context.Context, req *opsv1.DeleteServiceRequest) (*opsv1.DeleteServiceResponse, error) {
	if s.services == nil {
		return &opsv1.DeleteServiceResponse{Deleted: false}, nil
	}
	if _, ok, err := s.writableService(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return &opsv1.DeleteServiceResponse{Deleted: false}, nil
	}
	ok, err := s.services.Delete(ctx, req.GetName())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.DeleteServiceResponse{Deleted: ok}, nil
}

// PauseService scales a service to zero (remembering the prior count).
func (s *Service) PauseService(ctx context.Context, req *opsv1.PauseServiceRequest) (*opsv1.PauseServiceResponse, error) {
	svc, err := s.pauseResume(ctx, req.GetOwner(), req.GetName(), true)
	if err != nil {
		return nil, err
	}
	return &opsv1.PauseServiceResponse{Service: svc}, nil
}

// ResumeService restores a paused service to its pre-pause replica count.
func (s *Service) ResumeService(ctx context.Context, req *opsv1.ResumeServiceRequest) (*opsv1.ResumeServiceResponse, error) {
	svc, err := s.pauseResume(ctx, req.GetOwner(), req.GetName(), false)
	if err != nil {
		return nil, err
	}
	return &opsv1.ResumeServiceResponse{Service: svc}, nil
}

// ScaleService sets a service's desired replica count.
func (s *Service) ScaleService(ctx context.Context, req *opsv1.ScaleServiceRequest) (*opsv1.ScaleServiceResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	if req.GetReplicas() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "replicas must be >= 0")
	}
	if _, ok, err := s.writableService(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	out, err := s.services.Scale(ctx, req.GetName(), req.GetReplicas())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.ScaleServiceResponse{Service: toServiceInfo(out)}, nil
}

// GetServiceManifest returns the service's Deployment + Services as YAML.
func (s *Service) GetServiceManifest(ctx context.Context, req *opsv1.GetServiceManifestRequest) (*opsv1.GetServiceManifestResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	if _, ok, err := s.ownedService(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	y, err := s.services.Manifest(ctx, req.GetName())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.GetServiceManifestResponse{Yaml: y}, nil
}

// ApplyServiceManifest replaces a service from an edited multi-document YAML.
func (s *Service) ApplyServiceManifest(ctx context.Context, req *opsv1.ApplyServiceManifestRequest) (*opsv1.ApplyServiceManifestResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	if _, ok, err := s.writableService(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	out, y, err := s.services.ApplyManifest(ctx, req.GetName(), req.GetYaml(), req.GetDryRun())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	resp := &opsv1.ApplyServiceManifestResponse{Yaml: y}
	if !req.GetDryRun() {
		resp.Service = toServiceInfo(out)
	}
	return resp, nil
}

// RollbackService rolls a service's Deployment back to a prior revision
// (k8s `rollout undo`). revision=0 undoes to the previous revision.
func (s *Service) RollbackService(ctx context.Context, req *opsv1.RollbackServiceRequest) (*opsv1.RollbackServiceResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	if _, ok, err := s.writableService(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	svc, err := s.services.Rollback(ctx, req.GetName(), req.GetRevision())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error()).WithCause(err)
	}
	return &opsv1.RollbackServiceResponse{Service: toServiceInfo(svc)}, nil
}

// ServiceLogs reads a bounded window of a service's container log.
func (s *Service) ServiceLogs(ctx context.Context, req *opsv1.ServiceLogsRequest) (*opsv1.ServiceLogsResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	if _, ok, err := s.ownedService(ctx, req.GetOwner(), req.GetName()); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	tail := req.GetTailLines()
	if tail <= 0 {
		tail = s.serviceLogTail
	}
	lines, err := s.services.LogSource().Tail(ctx, req.GetName(), servicesmgr.LogOptions{
		TailLines: tail, Previous: req.GetPrevious(),
	})
	if err != nil {
		return nil, mapK8sErr(err)
	}
	resp := &opsv1.ServiceLogsResponse{Lines: lines, Available: len(lines) > 0}
	// A container that failed BEFORE producing any output (runc create error,
	// image pull failure, crash loop, unschedulable pod) has no stdout/stderr,
	// so the log stream is empty and the ONLY explanation lives in the pod
	// status. Attach it so the caller is not left with a bare "no logs".
	if len(lines) == 0 {
		s.fillPodDiagnostics(ctx, req.GetName(), &resp.PodPhase, &resp.Restarts, &resp.Message)
	}
	return resp, nil
}

// fillPodDiagnostics reads a service's pod diagnostics (phase / restarts /
// waiting-terminated reason+message) into the given out-params. Best-effort: on
// any error the out-params are left untouched.
func (s *Service) fillPodDiagnostics(ctx context.Context, name string, phase *string, restarts *int32, message *string) {
	if s.services == nil {
		return
	}
	svc, err := s.services.Get(ctx, name)
	if err != nil {
		return
	}
	if svc.PodPhase != "" {
		*phase = svc.PodPhase
	}
	*restarts = svc.Restarts
	if svc.Message != "" {
		*message = svc.Message
	}
}

// WatchServiceLogs streams a service's container log until the stream ends or the
// client disconnects.
func (s *Service) WatchServiceLogs(ctx context.Context, req *opsv1.WatchServiceLogsRequest, st opsv1connect.OpsServiceWatchServiceLogsServerStream) error {
	if s.services == nil {
		return errNoServiceBackend()
	}
	// A streaming interceptor cannot see the request message; apply the owner
	// check (user-token path) here.
	if err := checkOwner(ctx, req.GetOwner()); err != nil {
		return err
	}
	if _, ok, err := s.ownedService(ctx, req.GetOwner(), req.GetName()); err != nil {
		return err
	} else if !ok {
		return connect.NewError(connect.CodeNotFound, "service not found")
	}
	lines, errc := s.services.LogSource().Follow(ctx, req.GetName(), servicesmgr.LogOptions{
		Previous: req.GetPrevious(),
	})
	sent := false
	for l := range lines {
		sent = true
		if err := st.Send(&opsv1.WatchServiceLogsResponse{Output: l + "\n"}); err != nil {
			return err
		}
	}
	// Terminal frame: carry pod diagnostics when the container never produced
	// output, so a failed-to-start container is explained rather than blank.
	done := &opsv1.WatchServiceLogsResponse{Done: true}
	if !sent {
		s.fillPodDiagnostics(ctx, req.GetName(), &done.PodPhase, &done.Restarts, &done.Message)
	}
	if err := <-errc; err != nil {
		done.Error = err.Error()
	}
	return st.Send(done)
}

// ---- service helpers ----

// pauseResume is the shared dispatch for Pause/Resume.
func (s *Service) pauseResume(ctx context.Context, owner, name string, pause bool) (*opsv1.ServiceInfo, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	if _, ok, err := s.writableService(ctx, owner, name); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "service not found")
	}
	var out servicesmgr.Service
	var err error
	if pause {
		out, err = s.services.Pause(ctx, name)
	} else {
		out, err = s.services.Resume(ctx, name)
	}
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return toServiceInfo(out), nil
}

// ownedService resolves a service and reports whether it is visible to `owner`.
// A service owned by someone else is reported as not found.
func (s *Service) ownedService(ctx context.Context, owner, name string) (servicesmgr.Service, bool, error) {
	svc, err := s.services.Get(ctx, name)
	if err != nil {
		return servicesmgr.Service{}, false, nil // not found (or transient) => invisible
	}
	if !owned(svc.Creator, owner) {
		return servicesmgr.Service{}, false, nil
	}
	return svc, true, nil
}

// writableService resolves a service and reports whether `owner` may MUTATE it.
// An unowned service (creator "") is NOT writable (A1).
func (s *Service) writableService(ctx context.Context, owner, name string) (servicesmgr.Service, bool, error) {
	svc, err := s.services.Get(ctx, name)
	if err != nil {
		return servicesmgr.Service{}, false, nil
	}
	if !writable(svc.Creator, owner) {
		return servicesmgr.Service{}, false, nil
	}
	return svc, true, nil
}

// servicePorts resolves requested ports into servicesmgr.Port values. With no
// explicit ports it returns the default (tcp80 -> containerPort). Each distinct
// suffix becomes one k8s Service.
func servicePorts(specs []*opsv1.ServicePortSpec, containerPort int32) ([]servicesmgr.Port, error) {
	if len(specs) == 0 {
		target := containerPort
		if target == 0 {
			target = 8080
		}
		return []servicesmgr.Port{{Port: 80, Protocol: "tcp", TargetPort: target}}, nil
	}
	out := make([]servicesmgr.Port, 0, len(specs))
	publicPerSuffix := map[string]int{}
	for i, sp := range specs {
		port, proto, ok := servicesmgr.PresetPorts(sp.GetPreset())
		if !ok {
			return nil, fmt.Errorf("services[%d]: preset must be tcp80|tcp443|udp443", i)
		}
		suffix := sp.GetName()
		if port == 80 {
			publicPerSuffix[suffix]++
			if publicPerSuffix[suffix] > 1 {
				return nil, fmt.Errorf("services[%d]: only one tcp80 per service name", i)
			}
		}
		target := sp.GetTargetPort()
		if target == 0 {
			target = containerPort
		}
		if target == 0 {
			target = 8080
		}
		out = append(out, servicesmgr.Port{Suffix: suffix, Port: port, Protocol: proto, TargetPort: target})
	}
	return out, nil
}
