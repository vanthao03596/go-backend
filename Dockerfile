# syntax=docker/dockerfile:1

# Build stage. Runs on the build host's architecture and cross-compiles for
# the target so multi-arch builds do not need emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie AS build

WORKDIR /src

# Resolve modules first so the layer is reused while source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# Runtime stage. static-debian13 ships only ca-certificates, tzdata and a
# passwd file; :nonroot runs as uid 65532 with HOME=/home/nonroot. There is no
# shell, so the entrypoint must be in exec form.
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=build /out/api /api

# The app listens on PORT, default 8080 (internal/config).
EXPOSE 8080

ENTRYPOINT ["/api"]
