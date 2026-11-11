# syntax=docker/dockerfile:1.7
#
# Claude Code Docker image: native-agent-api + the native Claude Code CLI (pinned version + SHA-256).
# Built through docker/compose.yaml with CONTAINER_AGENT=claude, which passes the pinned base images
# and the build identity from the Docker env file (see docker/README.md).

ARG GO_IMAGE
ARG RUNTIME_IMAGE

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS gateway-build
ARG TARGETOS TARGETARCH VERSION COMMIT DATE
WORKDIR /src
RUN test -n "$VERSION" && test -n "$COMMIT" && test -n "$DATE"
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w \
        -X github.com/emreuyguc/native-agent-api/internal/buildinfo.Version=${VERSION} \
        -X github.com/emreuyguc/native-agent-api/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/emreuyguc/native-agent-api/internal/buildinfo.Date=${DATE}" \
      -o /out/native-agent-api ./cmd/native-agent-api

FROM ${RUNTIME_IMAGE} AS claude-download
ARG TARGETARCH
WORKDIR /download
SHELL ["/bin/ash", "-eo", "pipefail", "-c"]
COPY docker/claude/release.env ./
RUN set -eu; \
    . ./release.env; \
    case "$TARGETARCH" in \
        arm64) platform="linux-arm64-musl"; checksum="$CLAUDE_SHA256_ARM64" ;; \
        amd64) platform="linux-x64-musl"; checksum="$CLAUDE_SHA256_AMD64" ;; \
        *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    wget -q "https://downloads.claude.ai/claude-code-releases/${CLAUDE_VERSION}/${platform}/claude" -O claude; \
    echo "${checksum}  claude" | sha256sum -c -; \
    chmod 0755 claude

FROM ${RUNTIME_IMAGE}
LABEL org.opencontainers.image.title="native-agent-api-claude" \
      org.opencontainers.image.description="native-agent-api with the native Claude Code CLI"
# bash: Claude Code's Bash tool accepts only bash or zsh, and Codex prefers bash over sh.
# git: both CLIs use it to find the repository root, diffs and history. Projects that need more
# (language toolchains) extend this image. Packages come from the pinned Alpine release branch.
# hadolint ignore=DL3018
RUN apk add --no-cache bash git \
    && addgroup -S -g 65532 runner \
    && adduser -S -D -H -u 65532 -G runner -h /home/runner -s /bin/bash runner \
    && mkdir -p /home/runner/.claude /data /projects /tmp \
    && chown -R runner:runner /home/runner /data /projects /tmp \
    && chmod 0700 /home/runner /home/runner/.claude /data
COPY --from=gateway-build --chown=root:root /out/native-agent-api /usr/local/bin/native-agent-api
COPY --from=claude-download --chown=root:root /download/claude /usr/local/bin/claude
USER 65532:65532
WORKDIR /data
ENV HOME=/home/runner \
    USER=runner \
    TMPDIR=/tmp \
    PATH=/usr/local/bin:/usr/bin:/bin
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/native-agent-api"]
CMD ["serve"]
