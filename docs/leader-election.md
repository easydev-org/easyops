# Leader election for easyops background loops

## Problem

`easyops` runs background loops in-process:

- **sandbox reaper** (`internal/sandboxreaper`, started in `cmd/easyops`): deletes
  idle sandboxes every `SANDBOX_REAP_INTERVAL` (default 10m).
- (future) any other periodic reconciler.

With **one replica** this is fine. With **more than one replica** every replica
runs the loop, so idle sandboxes get reclaimed concurrently (redundant deletes,
races, wasted API calls) — and any future "act on the world" loop would be worse
(duplicate side effects).

The gateway already solved the same problem for its schedulers with
`internal/leader` (a **Postgres session advisory lock**). easyops, however, has
**no database** — it is a stateless k8s/Helm ops server. So the gateway's
mechanism does not transfer; we need a k8s-native equivalent.

## Requirements

1. **Exactly one** replica runs the loops at a time.
2. **Automatic failover**: if the leader pod dies / is evicted, a standby takes
   over within a bounded time (no manual step).
3. **Single-replica behaviour is unchanged**: a one-replica deployment must keep
   running the loops unconditionally (today's behaviour).
4. **No new heavy dependency**; reuse the k8s clients/RBAC easyops already has
   (it is the only component with cluster access).
5. Degrade safely: if the mechanism is unavailable (RBAC missing, no cluster),
   do **not** silently run the loops on every replica *and* do not silently stop
   them — log loudly and follow an explicit policy.

## Options

### A. `coordination.k8s.io/v1` Lease — **recommended**

The standard k8s leader-election primitive (`client-go/tools/leaderelection`).

- A `Lease` object (e.g. `easyops-leader` in the managed namespace) records
  `holderIdentity` + `renewTime`; the holder renews every `LeaseDuration/3`.
- A standby acquires when the lease expires (`LeaseDuration`, default 15s).
- `client-go` provides the whole loop (`leaderelection.RunOrDie` /
  `LeaderElector`) with `OnStartedLeading` / `OnStoppedLeading` callbacks — the
  exact shape we want: run the reaper inside `OnStartedLeading`, stop it on
  `OnStoppedLeading` (via a context cancel).

RBAC: add **one** rule to `k8s/rbac.yaml` (in `easy-vcs/deploy`):

```yaml
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "create", "update"]
```

Namespace-scoped, no cluster-wide access — consistent with the existing Role.

Cost: ~0 (the Lease is one small object; renewals are one PATCH every few
seconds). Failover: <= `LeaseDuration` (15s). `Identity` = the pod name (from the
downward API) so a restarted pod cannot hold a stale lease.

### B. ConfigMap/annotation "lock"

Do-it-yourself via an existing ConfigMap + optimistic concurrency
(`resourceVersion` CAS). Works, but re-implements Lease with worse semantics
(no built-in renew/expire/observability). Only worth it if we must avoid the
`coordination.k8s.io` RBAC — which we do not (we already own the Role). Not
recommended.

### C. Reuse the gateway's Postgres advisory lock

Would require easyops to gain a DB dependency purely for a lock — contradicts
"easyops is a stateless k8s ops server" and the dependency-light rule. Rejected.

## Recommended design (Option A)

Add `internal/leader` (k8s Lease based) + wire the reaper into it:

```
SANDBOX_LEADER_ELECTION=on|off      # default: off (today's behaviour)
SANDBOX_LEADER_LEASE=<name>         # default "easyops-leader"
SANDBOX_LEADER_LEASE_NS=<ns>        # default = SANDBOX_NAMESPACE
```

- `internal/leader.Elect(ctx, clientset, ns, name, identity, onLeader func(ctx))`
  wraps `leaderelection`; `onLeader` runs the reaper and returns when its ctx is
  cancelled (step-down).
- **Single replica**: election still works (it acquires immediately); behaviour
  is unchanged in effect (one holder). To keep the *pre-replica* behaviour
  byte-for-byte for deployments that never scale, `SANDBOX_LEADER_ELECTION=off`
  runs the reaper unconditionally (current path).

  > Recommendation: default **off** (today's behaviour) and let `easy-vcs/deploy`
  > turn it **on** once the RBAC rule is applied. This makes the change purely
  > additive and avoids a surprise "reaper stopped" if the Lease RBAC is missing.

- **Missing RBAC / no cluster**: `onLeader` is never invoked -> the loops would
  not run. This must be **loud**: log `warn: leader election enabled but the
  Lease could not be acquired (RBAC?); the reaper is NOT running`. (This is the
  safe failure: duplicate reclamation is worse than a temporarily idle reaper,
  and the boot RBAC self-check already flags the missing rule.)

## Migration / rollout

1. Add the `leases` rule to `k8s/rbac.yaml` in `easy-vcs/deploy` (deploy-owned).
2. easyops ships `internal/leader` + the env wiring; default `off` (no behaviour
   change), documented in README.
3. The platform flips `SANDBOX_LEADER_ELECTION=on` (and may raise replicas) once
   the RBAC is applied.
4. Verify: scale to 2 replicas, confirm exactly one logs
   `leader: acquired lease easyops-leader`, the other logs `leader: standby`;
   kill the leader, confirm the standby takes over within ~15s.

## Status

**Assessment only** — not implemented. Implementation is a separate MR (it needs
the `leases` RBAC from `easy-vcs/deploy` to be useful). Tracked here so the
decision + rollout are not re-derived.
