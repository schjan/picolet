FROM docker.io/library/golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build \
    -tags "remote,containers_image_openpgp,exclude_graphdriver_btrfs,btrfs_noversion,exclude_graphdriver_devicemapper" \
    -ldflags "-X main.version=${VERSION} -s -w" \
    -o /picolet ./cmd/picolet

FROM gcr.io/distroless/static:latest
COPY --from=builder /picolet /picolet
ENTRYPOINT ["/picolet"]
CMD ["run"]
