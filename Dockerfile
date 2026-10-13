# easyops server image. The generated protos are vendored under gen/; the k8s +
# helm deps come from the module proxy. Base images come from the deployment
# registry (artifact), configurable via REGISTRY.
#
# REGISTRY defaults to a NEUTRAL placeholder: at deploy time it must be set to
# the artifact Service host (`IMAGE_REGISTRY_HOST`) plus the namespace that
# holds the base images (e.g. `root` or `agent-toolchain`).
ARG REGISTRY=git.agent.svc.cluster.local/root
FROM ${REGISTRY}/golang:1.26-alpine AS build
ARG HTTP_PROXY
ARG HTTPS_PROXY
# Go module + checksum mirrors: the default proxy.golang.org / sum.golang.org
# are unreachable (or very slow) in CN networks. Default to the in-cluster
# artifact Go proxy.
# Override at build time for another proxy, e.g. `--build-arg GOPROXY=...`.
ARG GOPROXY=http://artifact-pvc.worker.svc.cluster.local/artifacts/go,direct
ARG GOSUMDB=off
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc \
    GOWORK=off \
    GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -mod=mod -trimpath -ldflags "-s -w" -o /out/easyops ./cmd/easyops

FROM ${REGISTRY}/alpine:3.24
# apk mirror: the default dl-cdn.alpinelinux.org is slow/unreliable from CN, so
# point apk at a domestic mirror. Override APK_MIRROR at build time to use
# another mirror.
ARG APK_MIRROR=https://mirrors.ustc.edu.cn/alpine
ARG HTTP_PROXY
ARG HTTPS_PROXY
# `apk` reads the lowercase http(s)_proxy variables.
ENV http_proxy=${HTTP_PROXY} https_proxy=${HTTPS_PROXY}
RUN sed -i "s|https\?://dl-cdn.alpinelinux.org/alpine|${APK_MIRROR}|g" /etc/apk/repositories \
    && apk add --no-cache ca-certificates
COPY --from=build /out/easyops /usr/local/bin/easyops
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/easyops"]
