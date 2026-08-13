# The per-node sidecar image. Built from the repo root as the build context, so
# the sidecarapi module this one resolves through a filesystem `replace` is
# present:
#
#   docker build -f sidecar/Dockerfile -t <ref> .
#
# GOWORK=off is set for the build because this module pins golang.org/x/crypto
# and google.golang.org/grpc *down* relative to the controller module. Under a
# workspace those replaces would be promoted to main-module status and leak into
# the root module's resolution; off keeps this build identical to a consumer's.
FROM docker.io/golang:1.26 AS build

ENV GOWORK=off
WORKDIR /workspace

# Manifests first so the dependency layer caches independently of source edits.
# Both modules are needed: sidecar/go.mod's `replace` target must exist before
# `go mod download` can resolve it.
COPY sidecar/go.mod sidecar/go.sum ./sidecar/
COPY sidecarapi/go.mod sidecarapi/go.sum ./sidecarapi/
RUN cd sidecar && go mod download

COPY sidecarapi/ ./sidecarapi/
COPY sidecar/ ./sidecar/

ARG TARGETOS
ARG TARGETARCH
RUN cd sidecar && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -o /go/bin/sei-sidecar .

FROM gcr.io/distroless/static-debian12
COPY --from=build /go/bin/sei-sidecar /usr/bin/sei-sidecar

# Compatibility shim, deliberately temporary. The controller renders
# `Command: []string{"seictl", "serve"}` into every SeiNode pod spec
# (internal/noderesource/noderesource.go, internal/task/bootstrap_resources.go).
# Controller rollouts and sidecar-image rollouts are independent, so a binary
# rename with no shim has no safe ordering: an old controller against a new
# image fails to find `seictl`, and a new controller against an old image fails
# to find the new name. Either way the pod does not crash loudly — the seid
# container blocks on a shell loop polling /v0/healthz behind a StartupProbe
# with FailureThreshold 86400 at 5s, so it hangs for days instead of alerting.
#
# With the shim, adopting this image is a pure `images.sidecar` reference
# change. Remove it only after the controller stops rendering `Command` (letting
# this ENTRYPOINT decide) and every cell has rolled past that controller.
COPY --from=build /go/bin/sei-sidecar /usr/bin/seictl

ENTRYPOINT ["/usr/bin/sei-sidecar"]
