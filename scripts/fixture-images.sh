#!/usr/bin/env bash
# Build and push the fixture images the integration suites pull from inside the
# cluster.
#
# Why a registry instead of `k3d image import`: sandboxmgr sets
# `imagePullPolicy: Always` (a long-lived tag must be re-resolved on every pod),
# so an imported image is re-pulled from Docker Hub and the pod fails. The
# fixtures therefore live in the k3d registry, exactly like a real deployment's
# artifact registry.
#
# Usage: ./scripts/fixture-images.sh
set -euo pipefail

REG="${EASYOPS_TEST_REGISTRY_HOST:?run scripts/k3d-up.sh first (it exports the registry)}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- the fake worker ----------------------------------------------------------
# sandboxmgr expects a worker listening on 48080 behind the Service on 80. The
# tests need something that listens there and can prove the caller reached the
# sandbox it created (by echoing WORKER_TOKEN), so busybox's httpd serving one
# generated index.html is enough and is far more reliable than `nc`.
cat >"$tmp/serve.sh" <<'EOF'
#!/bin/sh
# Log one startup line: busybox httpd does not log requests, and the suite
# asserts the pod-log path works, so there must be SOMETHING to read.
echo "worker starting port=${WORKER_PORT:-48080}"
mkdir -p /www
printf 'sandbox-ok token=%s\n' "${WORKER_TOKEN:-none}" > /www/index.html
exec httpd -f -p "${WORKER_PORT:-48080}" -h /www
EOF
cat >"$tmp/Dockerfile" <<'EOF'
FROM busybox:1.36
COPY serve.sh /serve.sh
RUN chmod +x /serve.sh
EXPOSE 48080
ENTRYPOINT ["/bin/sh", "/serve.sh"]
EOF

echo "building fixture images into ${REG}"
docker build -t "${REG}/easyops-fixtures/worker:1" "$tmp"

# --- the web service ----------------------------------------------------------
# servicesmgr needs a long-lived server that becomes READY (its readiness probe
# hits port 80), so reuse nginx under a registry-local name.
docker pull -q nginx:1.27-alpine
docker tag nginx:1.27-alpine "${REG}/easyops-fixtures/web:1"

for img in worker:1 web:1; do
  docker push -q "${REG}/easyops-fixtures/${img}"
done

echo "pushed:"
echo "  ${REG}/easyops-fixtures/worker:1"
echo "  ${REG}/easyops-fixtures/web:1"
