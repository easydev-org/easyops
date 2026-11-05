#!/usr/bin/env bash
# Create a throwaway k3d (k3s-in-Docker) cluster for the integration suites.
#
# It is deliberately a script rather than inline workflow YAML so it can be run
# locally with the same arguments CI uses:
#
#   ./scripts/k3d-up.sh easyops-ci
#
# k3d runs k3s inside a Docker container, so this needs a Docker daemon with
# cgroup v2 (the ubuntu-latest runner qualifies).
#
# A REGISTRY is created alongside the cluster. It is not optional: sandboxmgr
# pins `imagePullPolicy: Always` (a long-lived tag must be re-resolved on every
# pod), so a local `k3d image import` would be re-pulled from Docker Hub and
# fail. Tests therefore push their fixture images to this registry, which is
# also what the image-build suite will push to.
set -euo pipefail

CLUSTER="${1:-easyops-ci}"
REGISTRY="${2:-easyops-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
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

# --- registry -----------------------------------------------------------------
# k3d's own registry helper wires the containerd registry config into the
# cluster, so a pod can pull `<registry-host>/<repo>` over plain HTTP without a
# TLS setup. That is exactly what the fixture images and image builds need.
if k3d registry list -o json | grep -q "\"name\":\"k3d-${REGISTRY}\""; then
  echo "registry ${REGISTRY} already exists"
else
  k3d registry create "${REGISTRY}" --port "127.0.0.1:${REGISTRY_PORT}"
fi
# The name k3s resolves inside the cluster (k3d creates the DNS entry).
REGISTRY_INTERNAL="k3d-${REGISTRY}:${REGISTRY_PORT}"

# --- create the cluster -------------------------------------------------------
# --wait blocks until the server is Ready, so a later `kubectl` cannot race it.
# The k3s image is pinned: "latest" makes a red build unreproducible.
if k3d cluster list -o json | grep -q "\"name\":\"${CLUSTER}\""; then
  echo "cluster ${CLUSTER} already exists"
else
  k3d cluster create "${CLUSTER}" \
    --image "${K3S_IMAGE}" \
    --agents 1 \
    --registry-use "${REGISTRY_INTERNAL}" \
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

# The fixture images are built and pushed by scripts/fixture-images.sh AFTER
# this script has exported the registry host.
echo "registry (host):    127.0.0.1:${REGISTRY_PORT}"
echo "registry (cluster): ${REGISTRY_INTERNAL}"

KUBECONFIG_PATH="$(k3d kubeconfig write "${CLUSTER}")"
echo "kubeconfig: ${KUBECONFIG_PATH}"
echo "namespace:  ${NS}"
if [ -n "${GITHUB_ENV:-}" ]; then
  {
    echo "KUBECONFIG=${KUBECONFIG_PATH}"
    echo "EASYOPS_KUBECONFIG=${KUBECONFIG_PATH}"
    echo "EASYOPS_TEST_NAMESPACE=${NS}"
    # Tests build image refs from this host; it is the IN-CLUSTER name.
    echo "EASYOPS_TEST_REGISTRY=${REGISTRY_INTERNAL}"
    # The host-side name for docker push.
    echo "EASYOPS_TEST_REGISTRY_HOST=127.0.0.1:${REGISTRY_PORT}"
  } >>"${GITHUB_ENV}"
fi
