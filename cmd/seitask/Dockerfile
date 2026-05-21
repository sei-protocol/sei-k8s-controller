FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -ldflags="-s -w" -o seitask ./cmd/seitask/

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /workspace/seitask /seitask
# Bundle the runner subcommand's default templates at the conventional
# mount path. Scenario authors override by mounting a different ConfigMap
# at /templates/ — same contract as the legacy seitask-runner image.
COPY --from=builder /workspace/runner/templates /templates
USER 65532:65532

ENTRYPOINT ["/seitask"]
