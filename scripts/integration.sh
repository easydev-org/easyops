#!/usr/bin/env bash
# Run the easyops integration suites against a real cluster.
#
# The unit tests use a fake clientset, which cannot catch a kubelet-level
# failure (a rejected security context, an unwritable mount, a Service that does
# not resolve). This suite can, so CI runs it against k3d.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [ -z "${EASYOPS_KUBECONFIG:-}" ] && [ -z "${KUBECONFIG:-}" ]; then
  echo "no kubeconfig: run scripts/k3d-up.sh first" >&2
  exit 1
fi

# Diagnostics on failure: the tests report a pod reason, but the EVENTS are what
# explain a CreateContainerConfigError.
dump() {
  local ns="${EASYOPS_TEST_NAMESPACE:-easyops-ci}"
  echo "--- pods ---" >&2
  kubectl -n "$ns" get pods -o wide >&2 || true
  echo "--- events ---" >&2
  kubectl -n "$ns" get events --sort-by=.lastTimestamp >&2 || true
  echo "--- describe the managed pods ---" >&2
  kubectl -n "$ns" describe pods -l workspace/job=1 >&2 || true
}

# -count=1 defeats the test cache: an integration result must never be reused.
set +e
CGO_ENABLED=0 go test -tags=integration -count=1 -v -timeout 15m ./k8s/jobmgr/
rc=$?
set -e

if [ $rc -ne 0 ]; then
  dump
fi
exit $rc
