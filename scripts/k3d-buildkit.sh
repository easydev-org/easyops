#!/usr/bin/env bash
# Deploy buildkitd into the k3d cluster so the image-build suites can run REAL
# builds (not just the "backend unavailable" contract).
#
# Plain Kubernetes YAML on purpose: no Helm, no operator, nothing to install on
# the runner. buildkitd is a Deployment; the Go client dials it through a
# port-forward, because the test process runs on the HOST and a ClusterIP is not
# reachable from there.
set -euo pipefail

NS="${EASYOPS_TEST_NAMESPACE:-easyops-ci}"
REGISTRY_INTERNAL="${EASYOPS_TEST_REGISTRY:?run scripts/k3d-up.sh first}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
BUILDKIT_IMAGE="${BUILDKIT_IMAGE:-moby/buildkit:v0.18.2}"

command -v kubectl >/dev/null 2>&1 || { echo "missing: kubectl" >&2; exit 1; }

# ConfigMap FIRST: the Deployment mounts it, and a pod that starts before the
# ConfigMap exists would sit in ContainerCreating until it appears.
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: buildkitd-config
  namespace: ${NS}
data:
  buildkitd.toml: |
    # The k3d registry is plaintext; a build that pushes to it must not use TLS.
    [registry."${REGISTRY_INTERNAL}"]
      http = true
      insecure = true
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: buildkitd
  namespace: ${NS}
  labels: {app: buildkitd}
spec:
  replicas: 1
  selector:
    matchLabels: {app: buildkitd}
  template:
    metadata:
      labels: {app: buildkitd}
    spec:
      containers:
        - name: buildkitd
          image: ${BUILDKIT_IMAGE}
          args: ["--addr", "tcp://0.0.0.0:1234"]
          # buildkitd is PLATFORM infrastructure, not tenant code: it needs
          # privileged to set up its overlay snapshotter. Tenant isolation is the
          # job of what it BUILDS, not of the daemon.
          securityContext:
            privileged: true
          ports:
            - { name: grpc, containerPort: 1234 }
          readinessProbe:
            exec:
              command: ["buildctl", "--addr", "tcp://127.0.0.1:1234", "debug", "workers"]
            initialDelaySeconds: 2
            periodSeconds: 2
            failureThreshold: 60
          volumeMounts:
            - { name: buildkitd-config, mountPath: /etc/buildkit }
      volumes:
        - name: buildkitd-config
          configMap: { name: buildkitd-config }
---
apiVersion: v1
kind: Service
metadata:
  name: buildkitd
  namespace: ${NS}
  labels: {app: buildkitd}
spec:
  selector: {app: buildkitd}
  ports:
    - { name: grpc, port: 1234, targetPort: 1234 }
EOF

kubectl -n "${NS}" rollout status deployment/buildkitd --timeout=240s

# A red daemon otherwise surfaces as an opaque "solve failed" inside a test, so
# assert it answers BEFORE the suite runs.
kubectl -n "${NS}" run "buildkit-probe-$$" --rm -i --restart=Never \
  --image="${BUILDKIT_IMAGE}" --command -- \
  buildctl --addr tcp://buildkitd:1234 debug workers >/dev/null

# The Go client runs on the host, so bridge the ClusterIP.
kubectl -n "${NS}" port-forward svc/buildkitd 1234:1234 >/tmp/buildkitd-portforward.log 2>&1 &
FORWARD_PID=$!
trap 'kill ${FORWARD_PID} 2>/dev/null || true' EXIT
for _ in $(seq 1 60); do
  if (exec 3<>/dev/tcp/127.0.0.1/1234) 2>/dev/null; then
    exec 3<&- 3>&-
    break
  fi
  sleep 1
done

ADDR="tcp://127.0.0.1:1234"
echo "buildkitd ready at ${ADDR} (port-forwarded from buildkitd.${NS})"
if [ -n "${GITHUB_ENV:-}" ]; then
  {
    echo "EASYOPS_TEST_BUILDKIT_ADDR=${ADDR}"
    echo "EASYOPS_TEST_REGISTRY_PORT=${REGISTRY_PORT}"
  } >>"${GITHUB_ENV}"
fi
