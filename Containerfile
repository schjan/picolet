FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.1-trixie AS builder

WORKDIR /app

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,source=,target=.,rw \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -v \
    -mod=readonly -trimpath \
    -tags "remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" \
    -ldflags="-X main.version=${VERSION} -s -w" -o /picolet ./cmd/picolet

# Use the root variant (not :nonroot): picolet runs as UID 0 inside both rootless and rootful
# containers. In rootless Podman, UID 0 maps to the host user via user namespace — no real root
# privilege. The :nonroot default would be immediately overridden anyway and adds no benefit.
FROM gcr.io/distroless/static-debian13
COPY --from=builder /picolet /picolet
ENTRYPOINT ["/picolet"]
CMD ["run"]
