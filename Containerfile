FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.3-trixie AS builder

WORKDIR /app

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

ARG VERSION=dev
ARG GIT_SHA=unknown
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,source=.,target=.,rw \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -v \
    -mod=readonly -trimpath \
    -tags "remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" \
    -ldflags="-X github.com/schjan/picolet/pkg/version.Version=${VERSION} -X github.com/schjan/picolet/pkg/version.GitSHA=${GIT_SHA} -s -w" -o /picolet ./cmd/picolet

# Slim Debian with git: go-git's file:// transport shells out to git-upload-pack.
# TODO: go-git v6 removes this dependency — switch back to distroless/static-debian13.
# Runs as UID 0: in rootless Podman, maps to host user via user namespace — no real root privilege.
FROM docker.io/library/debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /picolet /picolet
ENTRYPOINT ["/picolet"]
CMD ["run"]
