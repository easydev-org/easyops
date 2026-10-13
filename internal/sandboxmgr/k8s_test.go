package sandboxmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/easy-platform/easyops/internal/runtimeprofiles"
)

func newTestClient() *Client {
	return NewWithClientset(fake.NewSimpleClientset(), Config{Namespace: "worker"})
}

func TestCreateSetsRuntimeClass(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	// A GPU profile renders a RuntimeClass; the pod must carry it, or the
	// driver devices are never mounted.
	_, _, err := c.Create(ctx, Spec{
		Name: "gpu", Image: "img:1", Creator: "alice",
		Runtime: runtimeprofiles.DefaultSettings().Render(false, 1),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("gpu"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "nvidia" {
		t.Fatalf("runtimeClassName = %v, want nvidia", pod.Spec.RuntimeClassName)
	}
	if got := pod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]; got.String() != "1" {
		t.Fatalf("gpu limit = %v", got)
	}
}

func TestCreateListResolveDelete(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()

	sb, token, err := c.Create(ctx, Spec{Name: "dev", Image: "img:1", Creator: "alice"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	if sb.Name != "dev" || sb.URL == "" {
		t.Fatalf("bad sandbox: %+v", sb)
	}

	// Secret carries the token.
	sec, err := c.cs.CoreV1().Secrets("worker").Get(ctx, resourceName("dev"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if string(sec.Data["token"]) != token {
		t.Fatalf("secret token mismatch")
	}

	// Pod has WORKER_TOKEN + port env.
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("dev"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod: %v", err)
	}
	envs := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envs[e.Name] = e.Value
	}
	if envs["WORKER_TOKEN"] != token || envs["WORKER_PORT"] != "48080" {
		t.Fatalf("bad env: %+v", envs)
	}
	if pod.Annotations[AnnoCreator] != "alice" {
		t.Fatalf("creator annotation missing")
	}

	// Service selects the pod.
	if _, err := c.cs.CoreV1().Services("worker").Get(ctx, resourceName("dev"), metav1.GetOptions{}); err != nil {
		t.Fatalf("service: %v", err)
	}

	// List.
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}

	// Resolve.
	url, tok, err := c.Resolve(ctx, "dev")
	if err != nil || tok != token || url != sb.URL {
		t.Fatalf("resolve: %v tok=%q url=%q", err, tok, url)
	}

	// Get.
	got, ok, err := c.Get(ctx, "dev")
	if err != nil || !ok || got.Name != "dev" {
		t.Fatalf("get: %v ok=%v", err, ok)
	}

	// Delete removes all three.
	ok, err = c.Delete(ctx, "dev")
	if err != nil || !ok {
		t.Fatalf("delete: %v ok=%v", err, ok)
	}
	if _, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("dev"), metav1.GetOptions{}); err == nil {
		t.Fatal("pod still present")
	}
	// Deleting again is a no-op, not an error.
	ok, err = c.Delete(ctx, "dev")
	if err != nil || ok {
		t.Fatalf("second delete: %v ok=%v", err, ok)
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	_, t1, err := c.Create(ctx, Spec{Name: "x", Image: "i"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second create with the same name must be REFUSED, not silently
	// overwrite (which would destroy the running sandbox and rotate its token).
	_, _, err = c.Create(ctx, Spec{Name: "x", Image: "i"})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("recreate err = %v, want ErrExists", err)
	}
	// The original sandbox and token survive untouched.
	got, ok, err := c.Get(ctx, "x")
	if err != nil || !ok {
		t.Fatalf("get after refused recreate: err=%v ok=%v", err, ok)
	}
	if got.Name != "x" {
		t.Fatalf("sandbox name = %q, want x", got.Name)
	}
	_, tok, err := c.Resolve(ctx, "x")
	if err != nil || tok != t1 {
		t.Fatalf("token rotated: err=%v tok=%q want %q", err, tok, t1)
	}
}

func TestResourceNameHashesInvalid(t *testing.T) {
	if resourceName("dev-01") != "wm-dev-01" {
		t.Fatalf("valid name mangled: %s", resourceName("dev-01"))
	}
	a := resourceName("Dev Team/One")
	b := resourceName("Dev Team/Two")
	if a == b || len(a) > 63 {
		t.Fatalf("invalid names not hashed distinctly: %s %s", a, b)
	}
}

func TestResolveMissing(t *testing.T) {
	c := newTestClient()
	if _, _, err := c.Resolve(context.Background(), "nope"); err == nil {
		t.Fatal("expected error for missing worker")
	}
}

func TestGetMissing(t *testing.T) {
	c := newTestClient()
	_, ok, err := c.Get(context.Background(), "nope")
	if err != nil || ok {
		t.Fatalf("get missing: err=%v ok=%v", err, ok)
	}
}

var _ = corev1.Secret{}

func TestCreateDefaultsRestartPolicyAndResources(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "def", Image: "img:1", Creator: "alice"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("def"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Default restart policy is Never: a sandbox that exits stays Failed.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	res := pod.Spec.Containers[0].Resources
	cpuReq, cpuLim := res.Requests[corev1.ResourceCPU], res.Limits[corev1.ResourceCPU]
	memReq, memLim := res.Requests[corev1.ResourceMemory], res.Limits[corev1.ResourceMemory]
	if got := cpuReq.String(); got != "500m" {
		t.Fatalf("cpu request = %q, want 500m", got)
	}
	if got := cpuLim.String(); got != "2" {
		t.Fatalf("cpu limit = %q, want 2", got)
	}
	if got := memReq.String(); got != "1Gi" {
		t.Fatalf("mem request = %q, want 1Gi", got)
	}
	if got := memLim.String(); got != "4Gi" {
		t.Fatalf("mem limit = %q, want 4Gi", got)
	}
}

func TestCreateExplicitResourcesKeepRequestEqualsLimit(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "ex", Image: "img:1", CPU: "250m", Memory: "512Mi"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("ex"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res := pod.Spec.Containers[0].Resources
	cpuReq, cpuLim := res.Requests[corev1.ResourceCPU], res.Limits[corev1.ResourceCPU]
	memReq, memLim := res.Requests[corev1.ResourceMemory], res.Limits[corev1.ResourceMemory]
	if cpuReq.String() != "250m" || cpuLim.String() != "250m" {
		t.Fatalf("cpu req/lim = %v/%v, want 250m/250m", cpuReq, cpuLim)
	}
	if memReq.String() != "512Mi" || memLim.String() != "512Mi" {
		t.Fatalf("mem req/lim = %v/%v, want 512Mi/512Mi", memReq, memLim)
	}
}
func TestCreateRestartPolicyOverride(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := NewWithClientset(cs, Config{Namespace: "worker", RestartPolicy: "Always"})
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "always", Image: "img:1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	pod, err := cs.CoreV1().Pods("worker").Get(ctx, resourceName("always"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Fatalf("restartPolicy = %q, want Always", pod.Spec.RestartPolicy)
	}
}

func TestFailedPodDiagnosticsAndWaitReady(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "oom", Image: "img:1", Creator: "alice"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force the pod into a Failed/OOMKilled state.
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("oom"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "worker", RestartCount: 3, Ready: false,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "OOMKilled", ExitCode: 137,
		}},
	}}
	if _, err := c.cs.CoreV1().Pods("worker").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Get surfaces the diagnostics.
	got, ok, err := c.Get(ctx, "oom")
	if err != nil || !ok {
		t.Fatalf("get: err=%v ok=%v", err, ok)
	}
	if got.Restarts != 3 {
		t.Fatalf("restarts = %d, want 3", got.Restarts)
	}
	if !strings.Contains(got.Message, "OOMKilled") {
		t.Fatalf("message = %q, want OOMKilled", got.Message)
	}
	// WaitReady fails FAST with the reason (does not burn the timeout).
	start := time.Now()
	err = c.WaitReady(ctx, "oom", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "OOMKilled") {
		t.Fatalf("WaitReady err = %v, want OOMKilled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("WaitReady took %s; should fail fast", elapsed)
	}
}

// TestServicePortFrontsWorker: the sandbox Service EXPOSES port 80 and targets
// the worker's own port (48080) — the worker listens on 48080, callers use :80.
func TestServicePortFrontsWorker(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "svc", Image: "img:1", Creator: "alice"}); err != nil {
		t.Fatal(err)
	}
	svc, err := c.cs.CoreV1().Services("worker").Get(ctx, resourceName("svc"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("ports = %+v", svc.Spec.Ports)
	}
	p := svc.Spec.Ports[0]
	if ServicePort != 80 {
		t.Fatalf("ServicePort = %d, want 80", ServicePort)
	}
	if p.Port != ServicePort {
		t.Fatalf("service port = %d, want %d", p.Port, ServicePort)
	}
	if p.TargetPort.IntVal != WorkerPort {
		t.Fatalf("targetPort = %d, want %d", p.TargetPort.IntVal, WorkerPort)
	}
	if dns := c.ServiceDNS(resourceName("svc")); !strings.HasSuffix(dns, ":80") {
		t.Fatalf("ServiceDNS = %q, want :80", dns)
	}
}

// TestGoldenDiskCacheMount: a Spec with GoldenDiskPVC mounts that PVC at
// GoldenDiskCachePath; without it, no such volume/mount is added.
func TestGoldenDiskCacheMount(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{
		Name: "vm", Image: "img:1", Creator: "alice", GoldenDiskPVC: "golden",
	}); err != nil {
		t.Fatal(err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("vm"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "golden" {
			found = true
		}
	}
	if !found {
		t.Fatalf("golden PVC volume not mounted: %+v", pod.Spec.Volumes)
	}
	mounted := false
	for _, mnt := range pod.Spec.Containers[0].VolumeMounts {
		if mnt.MountPath == GoldenDiskCachePath {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("cache not mounted at %s: %+v", GoldenDiskCachePath, pod.Spec.Containers[0].VolumeMounts)
	}
	// A plain sandbox (no GoldenDiskPVC) must NOT mount it.
	if _, _, err := c.Create(ctx, Spec{Name: "plain", Image: "img:1", Creator: "alice"}); err != nil {
		t.Fatal(err)
	}
	plain, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("plain"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range plain.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Fatalf("plain sandbox must not mount a PVC: %+v", v)
		}
	}
}

// TestSandboxImagePullPolicyAlways: the sandbox container pulls ALWAYS, so a
// rebuilt long-lived tag (e.g. sandbox-macos:base) is re-resolved on every pod
// instead of being served from the node image cache.
func TestSandboxImagePullPolicyAlways(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{Name: "pp", Image: "img:1", Creator: "alice"}); err != nil {
		t.Fatal(err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("pp"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pod.Spec.Containers[0].ImagePullPolicy; got != corev1.PullAlways {
		t.Fatalf("imagePullPolicy = %q, want Always", got)
	}
}

// TestSandboxVolumeMounts: extra PVCs are mounted with the requested
// read-only flag + subPath; a plain sandbox mounts none.
func TestSandboxVolumeMounts(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	if _, _, err := c.Create(ctx, Spec{
		Name: "vol", Image: "img:1", Creator: "alice",
		Volumes: []VolumeMount{
			{PVC: "data", MountPath: "/data"},
			{PVC: "ro", MountPath: "/ro", ReadOnly: true, SubPath: "sub"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	pod, err := c.cs.CoreV1().Pods("worker").Get(ctx, resourceName("vol"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Two PVC volumes + two mounts.
	pvcVols := 0
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			pvcVols++
		}
	}
	if pvcVols != 2 {
		t.Fatalf("pvc volumes = %d, want 2 (%+v)", pvcVols, pod.Spec.Volumes)
	}
	var rw, ro *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		m := &pod.Spec.Containers[0].VolumeMounts[i]
		switch m.MountPath {
		case "/data":
			rw = m
		case "/ro":
			ro = m
		}
	}
	if rw == nil || rw.ReadOnly {
		t.Fatalf("/data mount = %+v, want read-write", rw)
	}
	if ro == nil || !ro.ReadOnly || ro.SubPath != "sub" {
		t.Fatalf("/ro mount = %+v, want read-only subPath=sub", ro)
	}
}
