#!/usr/bin/env bash
# Build and push the easyops server image WITHOUT a local build daemon.
#
# Pipeline: buildctl (cluster buildkitd) -> docker archive -> skopeo -> the
# deployment registry. The platform has UNIFIED the image host onto artifact
# (`registry.host = artifact.<ns>.svc.cluster.local`): the SAME host serves both
# the base images (`root/golang`, `root/alpine`) and the pushed easyops image
# (`easy-vcs/easyops`). The registry/namespace/tag are env-configurable.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# artifact is the single image host (pull bases + push easyops).
REGISTRY="${REGISTRY:-artifact.worker.svc.cluster.local}"
NAMESPACE="${NAMESPACE:-easy-vcs}"
NAME="${NAME:-easyops}"
TAG="${TAG:-$(date +%Y%m%d%H%M%S)}"
DEST="${REGISTRY}/${NAMESPACE}/${NAME}:${TAG}"
BUILDKIT="${BUILDKIT_ADDR:-tcp://buildkitd.temp.svc.cluster.local:1234}"
# artifact write credential: user `root` + the artifact write token. The token
# defaults to the dev value (see easy-vcs/deploy:PUBLISHING.md); real deployments
# MUST override ARTIFACT_TOKEN.
ARTIFACT_USER="${ARTIFACT_USER:-root}"
ARTIFACT_TOKEN="${ARTIFACT_TOKEN:-dev-artifact-token}"
PROXY="${PROXY:-http://mihomo.develop.svc.cluster.local:7890}"
DOCKERFILE="Dockerfile"
# Base images live under <REGISTRY>/<BASE_NAMESPACE>/ (artifact serves `root`).
BASE_NAMESPACE="${BASE_NAMESPACE:-root}"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

echo "Building ${NAME} image -> ${DEST} (buildkitd=${BUILDKIT})"
buildctl --addr "${BUILDKIT}" build \
  --frontend dockerfile.v0 \
  --local "context=${DIR}" \
  --local "dockerfile=${DIR}" \
  --opt "filename=${DOCKERFILE}" \
  --opt "build-arg:REGISTRY=${REGISTRY}/${BASE_NAMESPACE}" \
  --opt "build-arg:HTTP_PROXY=${PROXY}" \
  --opt "build-arg:HTTPS_PROXY=${PROXY}" \
  --opt "build-arg:NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc" \
  --output "type=docker,name=${NAMESPACE}/${NAME}:${TAG},dest=${WORK}/image.tar" \
  --progress plain

echo "Pushing to ${DEST}"
skopeo copy \
  --dest-creds "${ARTIFACT_USER}:${ARTIFACT_TOKEN}" \
  --dest-tls-verify=false \
  "docker-archive:${WORK}/image.tar:${NAMESPACE}/${NAME}:${TAG}" \
  "docker://${DEST}"

echo "Verifying push:"
skopeo inspect --creds "${ARTIFACT_USER}:${ARTIFACT_TOKEN}" --tls-verify=false "docker://${DEST}" >/dev/null 2>&1 \
  && echo "OK ${DEST}" \
  || echo "inspect failed for ${DEST} (image may still be present)"
