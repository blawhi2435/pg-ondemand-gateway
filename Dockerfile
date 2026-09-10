# syntax=docker/dockerfile:1

# --- build stage ------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.23.3 AS build

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS=linux
ARG TARGETARCH=arm64

# CGO_ENABLED=0 for a fully static binary — no libc dependency at runtime,
# which is what lets the distroless "static" base image work below.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/pg-proxy ./cmd/pg-proxy

# --- runtime stage ------------------------------------------------------
# distroless/static: no shell, no package manager, just the binary and CA
# certs — the smallest reasonable attack surface for a network proxy.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/pg-proxy /pg-proxy

# :5432 speaks PostgreSQL wire protocol; :9090 serves /metrics, /healthz,
# /readyz (design §9.5, §9.4b) — these must stay on separate ports.
EXPOSE 5432 9090

USER nonroot:nonroot
ENTRYPOINT ["/pg-proxy"]
