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

# The subcommand belongs here, not in the pod spec. The controller renders no
# `Command` for the sidecar container, so this ENTRYPOINT is what runs, and the
# image owns its own binary name — renaming it later is an image-only change.
#
# `serve` is named explicitly rather than left as the root command's default: a
# bare `sei-sidecar` with SEI_HOME set prints help and exits 0, which under
# restartPolicy Always is a container that loops forever while reporting
# success. Harder to notice than a crash, and the seid container beside it would
# sit blocked on /v0/healthz behind a StartupProbe with FailureThreshold 86400.
ENTRYPOINT ["/usr/bin/sei-sidecar", "serve"]
