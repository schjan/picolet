FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.0-trixie AS builder

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

# Dedicated stage to fetch the pinned pass-cli binary. The version + per-arch
# SHA-256 hashes are manually pinned. To bump:
#   1. Fetch the new version metadata from https://proton.me/download/pass-cli/versions.json
#   2. Download pass-cli-linux-x86_64 and pass-cli-linux-aarch64 for that version
#   3. Run `sha256sum` on each; paste the values into PASS_CLI_SHA256_AMD64 / _ARM64
#   4. Verify the build still succeeds via `task build` and a container build smoke
FROM docker.io/library/debian:trixie-slim AS passcli
ARG TARGETARCH

ARG PASS_CLI_VERSION=2.0.2
ARG PASS_CLI_SHA256_AMD64=fd60a5041e642a7b1135ef6878c3d7ce3523b083210ea8100eee5c3701017ed9
ARG PASS_CLI_SHA256_ARM64=c12c531adb823d4eae4f6e69065c6e767b10ed71328a1b3fa625a4897ae3b38c

RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && case "$TARGETARCH" in \
        amd64) PP_ARCH=x86_64;  PP_SHA="$PASS_CLI_SHA256_AMD64" ;; \
        arm64) PP_ARCH=aarch64; PP_SHA="$PASS_CLI_SHA256_ARM64" ;; \
        *) echo "unsupported arch: $TARGETARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://proton.me/download/pass-cli/${PASS_CLI_VERSION}/pass-cli-linux-${PP_ARCH}" -o /pass-cli \
    && echo "${PP_SHA}  /pass-cli" | sha256sum -c - \
    && chmod +x /pass-cli \
    && /pass-cli --version

# Slim Debian with git: go-git's file:// transport shells out to git-upload-pack.
# TODO: go-git v6 removes this dependency — switch back to distroless/static-debian13.
# Runs as UID 0: in rootless Podman, maps to host user via user namespace — no real root privilege.
FROM docker.io/library/debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /picolet /picolet
COPY --from=passcli /pass-cli /usr/local/bin/pass-cli
ENTRYPOINT ["/picolet"]
CMD ["run"]
