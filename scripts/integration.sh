#!/usr/bin/env bash
# Run the easyops integration suites against a real cluster.
#
# The unit tests use a fake clientset, which cannot catch a kubelet-level
# failure (a rejected security context, an unwritable mount, a Service that does
# not resolve, a port mapping that is wrong). These suites can, so CI runs them
# against k3d.
#
# Usage: ./scripts/integration.sh [package...]
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [ -z "${EASYOPS_KUBECONFIG:-}" ] && [ -z "${KUBECONFIG:-}" ]; then
  echo "no kubeconfig: run scripts/k3d-up.sh first" >&2
  exit 1
fi

PKGS=("$@")
if [ ${#PKGS[@]} -eq 0 ]; then
  PKGS=(./k8s/sandboxmgr/ ./k8s/servicesmgr/ ./k8s/jobmgr/ ./internal/rpce2e/)
fi

# Diagnostics on failure: the tests report a pod reason, but the EVENTS are what
# explain a CreateContainerConfigError or an ImagePullBackOff.
dump() {
  local ns="${EASYOPS_TEST_NAMESPACE:-easyops-ci}"
  echo "--- pods ---" >&2
  kubectl -n "$ns" get pods -o wide >&2 || true
  echo "--- events ---" >&2
  kubectl -n "$ns" get events --sort-by=.lastTimestamp | tail -40 >&2 || true
  echo "--- registry ---" >&2
  kubectl -n "$ns" get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.status.containerStatuses[*].state}{"\n"}{end}' >&2 || true
}

# -count=1 defeats the test cache: an integration result must never be reused.
# -p 1 runs the packages SEQUENTIALLY: they share one cluster and one namespace,
# and the sandbox/service suites are timing-sensitive.
#
# A 40m budget: the build suite pushes several images through a cold buildkitd.
set +e
CGO_ENABLED=0 go test -tags=integration -count=1 -p 1 -v -timeout 40m "${PKGS[@]}"
rc=$?
set -e

if [ $rc -ne 0 ]; then
  dump
fi
exit $rc
