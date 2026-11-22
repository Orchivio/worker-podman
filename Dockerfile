# syntax=docker/dockerfile:1
# Cross-compiles on the build platform; no emulation needed for multi-arch.
FROM --platform=$BUILDPLATFORM golang:1.26.3-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/orchivio-worker ./cmd/orchivio-worker

# Static podman remote client matching the podman machine's server major version.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS podman
ARG TARGETARCH
ARG PODMAN_VERSION=v5.6.2
ARG PODMAN_SHA256_amd64=a602b2e0a671f130032bdb2120d02d931d97a400ced1777d3594e49d28d9d50a
ARG PODMAN_SHA256_arm64=0fd581e3eca585ca248be1ac254e56688b55387d92efe919bb6833b6db3e825b
ADD https://github.com/containers/podman/releases/download/${PODMAN_VERSION}/podman-remote-static-linux_${TARGETARCH}.tar.gz /podman.tar.gz
RUN eval "sum=\$PODMAN_SHA256_${TARGETARCH}" \
 && echo "${sum}  /podman.tar.gz" | sha256sum -c - \
 && tar -xzf /podman.tar.gz -C / \
 && mv /bin/podman-remote-static-linux_${TARGETARCH} /podman

# The worker needs git (clone, guarded push) and podman (sandboxes); the
# proxy subcommand runs from the same image.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=podman /podman /usr/local/bin/podman
COPY --from=build /out/orchivio-worker /usr/local/bin/orchivio-worker
ENV CONTAINER_HOST=unix:///run/podman/podman.sock
ENTRYPOINT ["orchivio-worker"]
CMD ["proxy"]
