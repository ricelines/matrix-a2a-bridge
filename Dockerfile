# syntax=docker/dockerfile:1.10

ARG GO_VERSION=1.25.0

FROM golang:${GO_VERSION}-trixie AS build

WORKDIR /src

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOMODCACHE=/go/pkg/mod \
    GOCACHE=/root/.cache/go-build

COPY --link go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go mod download

COPY --link cmd ./cmd
COPY --link internal ./internal

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go build \
        -buildvcs=false \
        -trimpath \
        -ldflags='-s -w' \
        -o /out/matrix-a2a-bridge \
        ./cmd/matrix-bot && \
    go build \
        -buildvcs=false \
        -trimpath \
        -ldflags='-s -w' \
        -o /out/mock-a2a \
        ./cmd/mock-a2a && \
    mkdir -p /out/data

FROM gcr.io/distroless/static-debian13:nonroot

WORKDIR /app

COPY --from=build --chown=nonroot:nonroot /out/matrix-a2a-bridge /app/matrix-a2a-bridge
COPY --from=build --chown=nonroot:nonroot /out/mock-a2a /app/mock-a2a
COPY --from=build --chown=nonroot:nonroot /out/data /app/data

ENTRYPOINT ["/app/matrix-a2a-bridge"]
