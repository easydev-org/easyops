#!/usr/bin/env bash
# Regenerate the Connect (Go) bindings for easyops.v1 from proto/.
#
# Requires: buf + protoc-gen-go + protoc-gen-connect-go on PATH.
#   go install github.com/bufbuild/buf/cmd/buf@v1.47.2
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
#   go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
#
# The generated output (gen/easyops/v1) is COMMITTED; run this after any proto
# change and commit the regenerated files.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT/proto"
buf generate --template buf.gen.easyops.yaml
echo "regenerated: gen/easyops/v1"
