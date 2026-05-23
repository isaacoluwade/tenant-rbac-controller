# syntax=docker/dockerfile:1.6
#
# Multi-stage build producing a distroless image.
# - Stage 1 compiles a static Go binary (CGO disabled).
# - Stage 2 copies just the binary into gcr.io/distroless/static:nonroot.
#
# The final image is ~30MB, runs as UID 65532, and has no shell.

FROM golang:1.22 AS builder

WORKDIR /workspace

# Cache module downloads.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Static build so we can use a distroless static base.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd

FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/manager /manager

USER 65532:65532

ENTRYPOINT ["/manager"]
