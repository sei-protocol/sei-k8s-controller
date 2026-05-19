FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -ldflags="-s -w" -o seitask-runner ./cmd/runner/

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /workspace/seitask-runner /seitask-runner
# Default per-kind templates. Workflow authors override by mounting a different
# ConfigMap at /templates/.
COPY --from=builder /workspace/runner/templates /templates
USER 65532:65532

ENTRYPOINT ["/seitask-runner"]
