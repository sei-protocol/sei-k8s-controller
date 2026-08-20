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
#
# --platform=$BUILDPLATFORM keeps the build stage on the runner's native
# architecture and cross-compiles with GOOS/GOARCH below. Without it, buildx
# emulates this stage under QEMU for every non-native target, which means
# compiling the whole sei-chain graph through emulation.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.26 AS build

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

# Compatibility shim, deliberately temporary. A pod spec's `command` overrides
# ENTRYPOINT outright, so this second name is what lets ONE image satisfy both
# controller versions:
#
#   controller at 92850d5 and earlier renders Command ["seictl","serve"],
#   which overrides the ENTRYPOINT below and resolves through this name.
#
#   controller at 186d65a and later renders no Command, so the ENTRYPOINT runs.
#
# Without it the two working combinations are diagonal — old controller needs
# `seictl`, new controller needs the ENTRYPOINT — so there is no ordering that
# passes through a working state, and image and controller must flip together in
# every cell at once. Worse, the rollback wedges: every mode's plan runs
# config-patch (a polled sidecar task) before replace-pod, so reverting the
# controller submits to a sidecar that is already dead, the submit fails,
# replace-pod is never reached, and the broken pod is never replaced. With this
# name present there is never a dead sidecar to wedge on.
#
# A second COPY rather than a symlink because the final stage is distroless and
# has no shell to run `ln`. The cost is one duplicated static binary in the
# image layer, taken deliberately over being clever across build stages.
#
# REMOVAL CONDITION (unchanged from when this was first written): remove only
# after the controller stops rendering Command — it already has, at 186d65a —
# AND every cell has rolled past that controller. As of this commit no cell has
# adopted this image at all, so the second half is not met anywhere. Deleting it
# before then is what turns three reversible steps into one simultaneous flip.
COPY --from=build /go/bin/sei-sidecar /usr/bin/seictl

ENTRYPOINT ["/usr/bin/sei-sidecar", "serve"]
