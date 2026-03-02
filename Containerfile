FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-trixie AS builder

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

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /picolet /picolet
ENTRYPOINT ["/picolet"]
CMD ["run"]
