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
# (internal/noderesource/noderesource.go, internal/task/bootstrap_resources.go),
# and a pod spec's `command` overrides ENTRYPOINT outright — so without this
# name the container cannot start at all.
#
# The rename cannot be done in lockstep, which is why the shim exists rather
# than a coordinated cutover. The sidecar image is chosen per node by
# `spec.sidecar.image` when set (noderesource.go, sidecarImage), and that
# overrides the platform config's images.sidecar — every SeiNode sample in
# manifests/samples pins it. So a controller that rendered the new name would
# break every node carrying a pinned older image, and no config rollout reaches
# those; each CR would have to change in the same instant as the controller
# deploy. The failure is also quiet: the seid container blocks on a shell loop
# polling /v0/healthz behind a StartupProbe with FailureThreshold 86400 at 5s,
# so a broken sidecar hangs for days rather than alerting.
#
# With both names present, adoption is a per-node image reference change at any
# pace. Retire the shim in three steps, each safe alone:
#
#  1. this image reaches every cell;
#  2. the controller stops rendering `Command` (or renders sei-sidecar) — by
#     then every image answers to either name;
#  3. delete this COPY.
COPY --from=build /go/bin/sei-sidecar /usr/bin/seictl

# `serve` belongs in the ENTRYPOINT, not left to the pod spec. Step 2 above
# removes `Command`, and a bare `sei-sidecar` with no subcommand prints help and
# exits 0 — a container that restarts forever while reporting success, which is
# worse than a crash. Naming the subcommand here makes step 2 a pure deletion.
ENTRYPOINT ["/usr/bin/sei-sidecar", "serve"]
