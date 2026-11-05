#!/usr/bin/env bash
# Create a throwaway k3d (k3s-in-Docker) cluster for the integration suite.
#
# It is deliberately a script rather than inline workflow YAML so it can be run
# locally with the same arguments CI uses:
#
#   ./scripts/k3d-up.sh easyops-ci
#
# k3d runs k3s inside a Docker container, so this needs a Docker daemon with
# cgroup v2 (the ubuntu-latest runner qualifies).
set -euo pipefail

CLUSTER="${1:-easyops-ci}"
K3D_VERSION="${K3D_VERSION:-v5.9.0}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.31.5-k3s1}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need docker
need curl

# --- install k3d (pinned) -----------------------------------------------------
if ! command -v k3d >/dev/null 2>&1; then
  echo "installing k3d ${K3D_VERSION}"
  curl -fsSL https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh \
    | TAG="${K3D_VERSION}" bash
fi
k3d version

# --- create the cluster -------------------------------------------------------
# --wait blocks until the server is Ready, so a later `kubectl` cannot race it.
# The k3s image is pinned: "latest" makes a red build unreproducible.
if k3d cluster list -o json | grep -q "\"name\":\"${CLUSTER}\""; then
  echo "cluster ${CLUSTER} already exists"
else
  k3d cluster create "${CLUSTER}" \
    --image "${K3S_IMAGE}" \
    --agents 1 \
    --wait \
    --timeout 180s \
    --k3s-arg "--disable=traefik@server:0" \
    --k3s-arg "--disable=servicelb@server:0" \
    --k3s-arg "--disable=metrics-server@server:0"
fi

k3d kubeconfig merge "${CLUSTER}" --kubeconfig-switch-context=false >/dev/null
kubectl cluster-info

# --- the namespace the suite uses --------------------------------------------
NS="${EASYOPS_TEST_NAMESPACE:-easyops-ci}"
kubectl create namespace "${NS}" --dry-run=client -o yaml | kubectl apply -f -

# k3d ships local-path provisioning, which the PVC test relies on. Assert it
# rather than assuming: a silent missing provisioner turns into a hanging test.
kubectl get storageclass

# A REACHABLE kubeconfig for the suite. k3d writes 0.0.0.0 by default, which is
# what a container-to-host client needs; the runner is the host here.
KUBECONFIG_PATH="$(k3d kubeconfig write "${CLUSTER}")"
echo "kubeconfig: ${KUBECONFIG_PATH}"
echo "namespace:  ${NS}"
if [ -n "${GITHUB_ENV:-}" ]; then
  {
    echo "KUBECONFIG=${KUBECONFIG_PATH}"
    echo "EASYOPS_KUBECONFIG=${KUBECONFIG_PATH}"
    echo "EASYOPS_TEST_NAMESPACE=${NS}"
  } >>"${GITHUB_ENV}"
fi
