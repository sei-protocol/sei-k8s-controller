FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
# The root module resolves sidecarapi through a filesystem `replace`, so its
# go.mod must be present before `go mod download` — otherwise the replace target
# is missing and the prefetch fails. Copy the manifests only, so this layer
# still caches on dependency changes rather than on every source edit.
COPY sidecarapi/go.mod sidecarapi/go.mod
COPY sidecarapi/go.sum sidecarapi/go.sum
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager ./cmd/

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
