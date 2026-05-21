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
# Runner templates (SeiNodeTask CRs) at the conventional mount path.
COPY --from=builder /workspace/runner/templates /templates
# Per-scenario SND templates (consumed by provision-snd's --template).
# Add a COPY line per scenario; Workflow YAMLs reference paths under
# /scenarios/<scenario>/.
COPY --from=builder /workspace/scenarios/release-test /scenarios/release-test
USER 65532:65532

ENTRYPOINT ["/seitask"]
