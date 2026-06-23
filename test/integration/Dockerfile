# The integration test suite compiled to an image: the build-tagged test binary,
# run by one in-cluster CronJob per target (args: -test.run TestX). The suites
# carry their fault/seiload templates via //go:embed, so the binary is
# self-contained (no extra files to COPY).
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY . .

# `go test -c` compiles the suite to a standalone binary whose entrypoint runs
# the selected test; the integration build tag is what gates the suites into it.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go test -c -tags integration -ldflags="-s -w" -o harness.test ./test/integration/

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /workspace/harness.test /harness.test
USER 65532:65532

# A CronJob selects a suite + budget via args, e.g.
#   ["-test.run", "TestBenchmark", "-test.v", "-test.timeout", "0"]
ENTRYPOINT ["/harness.test"]
