# easyops

**easyops is the third core server** (alongside [`easyvcs`](../../easyvcs) and
[`artifact`](../../artifact)): the Kubernetes / BuildKit / Helm **ops** backend
that the workspace gateway calls over the cluster network.

It has **two layers**:

| Layer | Path | What it is |
|---|---|---|
| **Library** | `internal/*` | Plain Go packages that render and manage cluster objects. They know nothing about tenants, sessions, VCS, presets or the agent protocol. |
| **Service** | `cmd/easyops` + `proto/easyops/v1` | The Connect **`easyops.v1.OpsService`** server that exposes the library over the network. This is the ONLY component that needs Kubernetes permissions. |

The gateway keeps the *policy* (tenant auth, session binding, immutable presets,
the `workspace.v1` RPC surface) and calls `OpsService` for the *cluster work* —
it does **not** link the library and needs **no** cluster RBAC of its own.

## Status

Phases 1–4 are done: the whole ops library is migrated and the **full
`OpsService` surface is implemented**. The gateway's Phase 5 (switch to the
network API) is the remaining step.

## Library packages

| Package | Purpose | Status |
|---|---|---|
| `internal/runtimeprofiles` | Maps the two caller knobs (`kvm`, `gpu_count`) to pod-level device requests, mounts, caps, resources and RuntimeClass. `Privileged` is always forced `false`. | **migrated** |
| `internal/sandboxmgr` | Sandbox Kubernetes backend: Pod + Service + Secret triples in one namespace. | **migrated** |
| `internal/k8scheck` | Non-fatal boot-time RBAC self-check via `SelfSubjectAccessReview`. | **migrated** |
| `internal/servicesmgr` | Long-lived Deployment + Service: deploy / lifecycle / scale / manifest / rollout rollback, PVCs, logs, live watch. | **migrated** |
| `internal/k8swatch` | Coalesced Kubernetes watch primitive (many resources → one "changed" signal). | **migrated** |
| `internal/imagebuild` | `buildctl` against the cluster buildkitd → push to the registry. | **migrated** |
| `internal/helmmgr` | Helm SDK templating + strict namespaced-kind apply, release history in ConfigMaps. | **migrated** |
| `internal/sandboxreaper` | Idle-sandbox reclamation. Pure library: the worker client is injected via `NewWorker` (interface), so it never imports the worker protocol. | **migrated** |
| `internal/opssvc` | **Service adapter** (not a library): maps `OpsService` RPCs onto the library, enforces the service token, stamps/filters `owner`, and hosts the concrete worker client. The only `internal` package that imports `connectrpc`. | **new** |

## The `OpsService` contract

`proto/easyops/v1/ops.proto` defines `easyops.v1.OpsService`. The full surface:

- **sandbox**: `CreateSandbox` / `ListSandboxes` / `GetSandbox` /
  `ResolveSandbox` (returns url+token) / `DeleteSandbox` / `SandboxLogs` /
  `WatchSandboxLogs` (stream). A sandbox is owned by easyops, so only easyops'
  ServiceAccount can read its Pod — `SandboxLogs` (owner-scoped) is how a
  non-admin caller diagnoses a `Failed` sandbox. `GetSandbox`/`ListSandboxes`
  carry pod diagnostics (phase, restarts, `exit_code`, `terminated_reason`,
  `last_state_reason`, container, start/finish times) and `SandboxLogs` also
  attaches recent Kubernetes events (best-effort; needs the `events` read RBAC).
- **services**: `DeployService` / `ListServices` / `GetService` / `DeleteService`
  / `PauseService` / `ResumeService` / `ScaleService` / `GetServiceManifest` /
  `ApplyServiceManifest` / `RollbackService` / `ServiceLogs` /
  `WatchServiceLogs` (stream)
- **pvc**: `CreatePVC` / `ListPVCs` / `GetPVC` / `DeletePVC`
- **helm**: `HelmDeploy` / `HelmList` / `HelmHistory` / `HelmRollback` /
  `HelmUninstall`
- **image**: `BuildImage` (repo Dockerfile → buildkit → registry) / `ImportImage`
  (`FROM <source>` re-push)
- **live**: `WatchWorkspace(stream)` — an initial full snapshot, then a frame
  whenever any owned **sandbox, service, PVC or Helm release** changes. **Every
  frame is self-contained** — it carries all owner-filtered lists (`sandboxes` /
  `services` / `pvcs` / `helm_releases`) — while the per-kind bits
  (`sandboxes_changed` / `services_changed` / `pvcs_changed` /
  `helm_releases_changed`) flag which kind(s) changed this frame (all configured
  kinds true on the snapshot frame; a kind whose backend is absent stays false).
  Fields `services`=3 / `pvcs`=4 / `*_changed`=5..7 / `helm_releases`=8 /
  `helm_releases_changed`=9 were **added** to the original message (fields 1/2
  unchanged) — a backward-compatible extension so the gateway can map it straight
  onto `workspace.v1`'s stream.
- **health**: `/healthz` (liveness, process only) and `/readyz` (readiness: does
  a `k8scheck` probe reach the Kubernetes API)
Helm and image RPCs take the **repo tarball in the request** (`archive` bytes), so
easyops needs **no VCS access** — the gateway fetches the archive from EasyVCS and
passes the bytes.

### Auth & owner (unified tenant identity)

Every RPC requires `Authorization: Bearer <token>`; a missing or invalid token is
rejected with **401** (`CodeUnauthenticated`) on unary and streaming calls alike.
**Two token kinds** are accepted:

1. **Shared service token** (`EASYOPS_SERVICE_TOKEN`) — the cluster-internal
   caller (the gateway). A **trusted** caller: any `owner` is allowed.
2. **User token** — verified against **EasyVCS** via `platform/identity`
   (`EASYOPS_EASYVCS_URL`, e.g. `http://easyvcs-server.worker.svc.cluster.local`).
   On this path the request's `owner` **must equal** the token's username
   (defense in depth); a mismatch is rejected with **403**
   (`CodePermissionDenied`). Without `EASYOPS_EASYVCS_URL` only the service token
   is accepted (user tokens are 401).

Every request carries an opaque `owner`. easyops **stamps** it onto the created
object (`Spec.Creator` / annotation) and **filters** `List*`/`Get*`/`Delete*` by
it. An **empty** owner means "no filter"; an object owned by another owner is
reported as not-found (never leaked). A backend that is **not configured** makes
its RPCs return `CodeUnavailable` — the process still starts.

### Observability

Prometheus metrics are served on **`METRICS_PORT`** (default `9090`), a
**separate port** from the OpsService/healthz mux, so it never contends with
`PORT`. Every OpsService RPC is recorded (`rpc_requests_total`,
`rpc_duration_seconds`, labelled by `proc` + `code`) via `platform/obs`.

### Error mapping (k8s backend errors)

Backend/kubernetes errors are mapped to a meaningful Connect code so a caller
can tell **permission** from **failure** (the gateway's error handling and
diagnosis depend on this). The mapping lives in `internal/opssvc/errs.go`
(`mapK8sErr`):

| Backend error | Connect code | HTTP |
|---|---|---|
| `Forbidden` | `CodePermissionDenied` | 403 |
| `Unauthorized` | `CodeUnauthenticated` | 401 |
| `NotFound` | `CodeNotFound` | 404 |
| `AlreadyExists` | `CodeAlreadyExists` | 409 |
| `Conflict` | `CodeAborted` | 409 |
| anything else | `CodeInternal` | 500 |

Semantic mappings are kept **unchanged**: bad parameters → `CodeInvalidArgument`;
an unconfigured backend → `CodeUnavailable` (503); `sandboxmgr.ErrExists` →
`CodeAlreadyExists`; a mounted-PVC delete → `CodeFailedPrecondition`; a
`CreateSandbox` ready timeout → `CodeDeadlineExceeded`; `Get*` not-found still
returns `{found:false}` (not 404).

A single-kind list failure inside `WatchWorkspace` does **not** abort the stream:
the kind is left empty and a warning is logged, while the other kinds keep
streaming.

## Deploy

Platform runtime manifests (the easyops Deployment + Service + `easyops-secrets`
Secret, and its dedicated ServiceAccount + `worker`-namespace Role/RoleBinding)
are **centralized in [`easy-vcs/deploy`](../../deploy)** — the Helm chart
`easyvcs-platform` (`templates/easyops.yaml`, `templates/rbac.yaml`). This repo
only maintains the **code + `Dockerfile` + `build-image.sh`**.

- `Dockerfile` / `build-image.sh` — build + push the server image to artifact.
- Env: `PORT`, `SANDBOX_NAMESPACE`, `WORKSPACE_RUNTIME`, `DEFAULT_SANDBOX_IMAGE`,
  `EASYOPS_SERVICE_TOKEN`, `SANDBOX_IDLE_TTL`, `SANDBOX_REAP_INTERVAL`,
  `PVC_STORAGE_CLASS`, `PVC_DEFAULT_SIZE`, `SERVICE_LOG_TAIL`, and (for image
  builds) `BUILDKIT_ADDR`,
  `IMAGE_REGISTRY_HOST` / `IMAGE_REGISTRY_SCHEME` /
  `IMAGE_REGISTRY_USER` / `IMAGE_REGISTRY_PASSWORD`.
  - **Sandbox resources / restart policy.** `SANDBOX_CPU_REQUEST` /
    `SANDBOX_CPU_LIMIT` / `SANDBOX_MEMORY_REQUEST` / `SANDBOX_MEMORY_LIMIT`
    override the split defaults (requests `500m`/`1Gi`, limits `2`/`4Gi` — a
    Burstable pod; an explicit sandbox `cpu`/`memory` still sets request == limit).
    `SANDBOX_RESTART_POLICY` defaults to **`Never`**: a sandbox that exits
    (OOMKilled / crash) stays `Failed` with a readable reason instead of silently
    restarting. Service containers use the analogous `SERVICE_CPU_REQUEST` /
    `SERVICE_CPU_LIMIT` / `SERVICE_MEMORY_REQUEST` / `SERVICE_MEMORY_LIMIT`.
- **Package upstream (all packages via artifact).** When
  `SANDBOX_PACKAGE_UPSTREAM` is set (the artifact base URL, e.g.
  `http://artifact-pvc.worker.svc.cluster.local`), easyops makes a fresh sandbox
  fetch packages from artifact with **no per-session setup**, in two
  complementary halves:
  - **env half** — the standard-env subset (`ARTIFACT_UPSTREAM`,
    `PIP_INDEX_URL`/`PIP_TRUSTED_HOST`, `NPM_CONFIG_REGISTRY`,
    `GOPROXY`/`GOSUMDB=off`, `HEX_MIRROR`, `PUB_HOSTED_URL`) is merged **under**
    the caller's `env` (an explicit key wins) and injected into the sandbox Pod.
  - **file half (bootstrap)** — after the sandbox is created, easyops runs a
    one-shot **setup job** on the worker (`Execute`) that writes the
    file-configured tools' configs (apt/apk, maven/gradle, cargo, rubygems,
    composer, nuget, conda, git) pointing at artifact. It is **idempotent**
    (every write overwrites), **best-effort** (a failure is logged, never fails
    sandbox creation) and gated by `SANDBOX_BOOTSTRAP=on|off` (default **on**
    when the upstream is set). Unset upstream = nothing injected (the previous
    behaviour).
- **Image guard.** A **sandbox** image must come from the artifact registry
  (`IMAGE_REGISTRY_HOST`) **and** the dedicated `SANDBOX_ORG` (default
  `sandbox`); a bare `<org>/<name>:<tag>` is rewritten to the configured
  registry. A **service** image must come from artifact (any org). Both guards
  are **disabled** when `IMAGE_REGISTRY_HOST` is unset (the deployment opted
  out), preserving the previous behaviour.
- **Identity vs. namespace (explicit kubeconfig).** easyops builds its cluster
  clients from a shared REST config (see `internal/k8sconfig`): the identity
  comes **only** from `EASYOPS_KUBECONFIG` — `EASYOPS_KUBECONFIG` → **error**.
  Neither `KUBECONFIG` nor the in-cluster ServiceAccount is consulted: easyops
  never acts as the Pod's ServiceAccount and never picks up an ambient
  `KUBECONFIG`. This lets a deployment **run in namespace A but act with
  namespace B's identity**: Kubernetes forbids a Pod from referencing a
  ServiceAccount in another namespace, so when the identity/permissions live in B
  while the Pod runs in A, inject a kubeconfig (whose identity is B's) and set
  `EASYOPS_KUBECONFIG` to its path. The **operated namespace** is orthogonal and
  still comes from `SANDBOX_NAMESPACE`. A deployment must also NOT mount a
  ServiceAccount token (the deploy chart sets `automountServiceAccountToken:
  false`).

  > **Operational pitfall — a bad kubeconfig silently keeps the OLD pod serving.**
  > easyops **fails fast** (`log.Fatalf`) when `EASYOPS_KUBECONFIG` is missing or
  > malformed (e.g. a hand-copied CA/token: `illegal base64 data at input byte …`).
  > On `helm upgrade` that makes the **new pod CrashLoopBackOff**, so the
  > Deployment **keeps the previous pod running**. The rollout looks "done" while
  > the OLD binary keeps serving — RPCs succeed but silently lack newer behavior
  > (e.g. `CreateSandbox{ golden_disk }` ignored, the sandbox Service still on the
  > old port). **Always verify the RUNNING pod's version, not just the chart:**
  > `kubectl -n <ns> get deploy easyops -o wide` / `get rs,pods -l app=easyops`
  > / `rollout status` — and the pod log's first line
  > (`k8sconfig: using EASYOPS_KUBECONFIG=… identity=…`; a crash shows the
  > `k8sconfig:` error). Prefer `insecure-skip-tls-verify` over an inline CA in
  > `helm --set` values so the kubeconfig cannot be corrupted in transit.

A healthy deployment logs **no** `rbac: ... DENIED` line at boot.

## Boundary

**In scope (ops, belongs here):** sandbox / service / helm / image / PVC
lifecycle; runtime capability rendering (KVM/GPU, never `privileged`); RBAC
self-check; idle reclamation; raw k8s watch primitives; the `OpsService` network
surface. Its **runtime manifests live in `easy-vcs/deploy`** (the
`easyvcs-platform` chart) — this repo no longer holds platform manifests.

**Out of scope (stays in `agent-gateway`):** tenants, sessions, roles, immutable
presets — all policy; the `workspace.v1` surface and `gen/workspace/**`; EasyVCS /
agent clients; auth/tokens/`X-Session-Name` binding; the local sqlite store;
`internal/idlewatch` (agent + NATS session semantics).

## Import-graph verdict

**Verified**: easyops imports **no gateway/business package**. The library
packages import only stdlib + `k8s.io/*` (+ `sigs.k8s.io/yaml`, + `helm.sh/helm`
in `helmmgr`); `connectrpc` appears **only** in `cmd/easyops`, `gen/easyops/v1`
and the `internal/opssvc` adapter.

```sh
# banned imports anywhere in the closure (must be empty)
go list -deps ./... | grep -Ei \
  'agent-gateway|gen/workspace|internal/roles|internal/easyvcs|internal/agentclient|modernc.org/sqlite|nats-io|abc-protocol'
# -> (no output)

# connectrpc must NOT reach the library packages (must be empty)
go list -deps ./internal/runtimeprofiles ./internal/sandboxmgr ./internal/servicesmgr \
  ./internal/k8swatch ./internal/k8scheck ./internal/imagebuild ./internal/helmmgr \
  ./internal/sandboxreaper | grep connectrpc
# -> (no output)
```

See [`DEVELOP.md`](./DEVELOP.md) for the full rules and the migration record.

## Build & test

```sh
gofmt -l .        # must be empty
go build ./...
go vet ./...
go test ./...

# regenerate the OpsService bindings (requires buf + the two plugins)
bash scripts/gen-proto.sh
```

## Migration plan

1. **Phase 1 (done).** `runtimeprofiles` + `sandboxmgr` (+ `k8scheck`).
2. **Phase 2 (done).** `servicesmgr` + `k8swatch`.
3. **Phase 3 (done).** `imagebuild` + `helmmgr` (+ `helm.sh/helm/v3`).
4. **Phase 4 (done).** `sandboxreaper`. (The platform manifests moved to
   [`easy-vcs/deploy`](../../deploy)'s `easyvcs-platform` chart.)
5. **Phase 4.5 (done).** `cmd/easyops` + `proto/easyops/v1` + `internal/opssvc`:
   the full `OpsService` surface (sandbox / services / pvc / helm / image + live).
6. **Phase 5 (next, gateway).** `agent-gateway` switches to calling `OpsService`
   over the network and drops the easyops library dependency (and its cluster RBAC).

Import paths are rewritten `agent-gateway/internal/...` → `easyops/internal/...`;
all exported library symbols and signatures are unchanged.
