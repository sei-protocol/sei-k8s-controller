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

# Compatibility shim, deliberately temporary. A pod spec's `command` REPLACES
# the image ENTRYPOINT (and `args` replaces CMD); neither render site sets Args,
# so argv is exactly what the controller renders. This second name is therefore
# what lets ONE image satisfy both controller versions:
#
#   controller <= 92850d5 renders Command ["seictl","serve"], which replaces the
#   ENTRYPOINT below and resolves through this name via PATH (/usr/bin is on the
#   distroless PATH).
#
#   controller >= 186d65a renders no Command, so the ENTRYPOINT below runs.
#
# The asymmetry matters: this makes the IMAGE valid under both controllers. It
# does not make the new controller valid with the OLD image, and cannot — that
# pair runs a bare `seictl`, which prints help and exits 0, looping under
# restartPolicy Always while seid sits blocked on /v0/healthz. So the cutover has
# a required order, per cell: adopt this image FIRST, bump the controller SECOND.
# The safe rollback is "revert the controller, leave the image", never the
# reverse.
#
# Because images.sidecar is read once at controller startup, adopting the image
# means restarting the old controller, and that restart trips sidecarImageDrifted
# and rolls the cell through replace-pod — on the old controller, through this
# name. The later controller bump changes no image, so it drifts nothing and
# rolls nothing. StatefulSets are OnDelete, so live pods keep the old template's
# Command while any recreated pod carries none; this name is what makes both
# revisions runnable in one cell at one time.
#
# Why it must not be removed early: once a node's sidecar is dead, any LATER
# image-drift plan wedges at config-patch, which every mode orders before
# replace-pod and which submits to that dead sidecar. The pod is then never
# replaced and recovery is a manual delete. Keeping this name means the revert
# leaves a live sidecar behind, so that state is not reachable by rolling back.
#
# A second COPY rather than a symlink: the build stage could `ln -s` and this
# stage could COPY the link, saving a duplicated binary, but that is untested
# here and the duplicate is a transitional cost we would rather pay than be
# clever across stages for a shim scheduled for deletion.
#
# REMOVAL CONDITION — check the pods, not the controller version, because
# OnDelete lets a pod created by the old controller outlive the controller bump:
#   1. no rendered pod spec names `seictl` — StatefulSet template AND bootstrap
#      Job, both render sites; and
#   2. no cell's controller-config still points images.sidecar at ghcr.
# Per-node confirmation that a roll landed is .status.currentSidecarImage.
# Deleting this makes a controller rollback below 186d65a a one-way door unless
# the image is rolled back with it.
COPY --from=build /go/bin/sei-sidecar /usr/bin/seictl

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
