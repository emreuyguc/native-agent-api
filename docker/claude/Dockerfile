# syntax=docker/dockerfile:1.7
#
# Claude Code Docker image: native-agent-api + the native Claude Code CLI (pinned version + SHA-256).
# Built through docker/compose.yaml with CONTAINER_AGENT=claude (see docker/README.md). The base
# images are pinned here by tag and digest; update both together, in every Dockerfile.

FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS gateway-build
ARG TARGETOS TARGETARCH
WORKDIR /src
SHELL ["/bin/ash", "-eo", "pipefail", "-c"]
# git reads the build identity from the checkout, like `make build`. The package comes from the
# pinned Alpine release branch.
# hadolint ignore=DL3018
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
# The version is the nearest tag and commit; -dirty marks changes to tracked files in the built
# sources, like `git describe --dirty`. Untracked files do not count: .gitignore is not in the
# build context.
RUN --mount=type=bind,source=.git,target=.git \
    --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    export GIT_OPTIONAL_LOCKS=0; \
    git config --global --add safe.directory /src; \
    version=$(git describe --tags --always); \
    if [ -n "$(git status --porcelain --untracked-files=no -- go.mod go.sum cmd internal)" ]; then version="$version-dirty"; fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags="-s -w \
        -X github.com/emreuyguc/native-agent-api/internal/buildinfo.Version=${version} \
        -X github.com/emreuyguc/native-agent-api/internal/buildinfo.Commit=$(git rev-parse HEAD) \
        -X github.com/emreuyguc/native-agent-api/internal/buildinfo.Date=$(git show -s --format=%cI HEAD)" \
      -o /out/native-agent-api ./cmd/native-agent-api

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS claude-download
ARG TARGETARCH
WORKDIR /download
SHELL ["/bin/ash", "-eo", "pipefail", "-c"]
# The Claude Code release the adapter is tested against, with the SHA-256 of each platform's
# binary. Update all three together with TestedVersion in internal/agent/claude.
RUN set -eu; \
    cli_version=2.1.318; \
    case "$TARGETARCH" in \
        arm64) platform="linux-arm64-musl"; checksum=a5ec862f882101dbe3e959ec166ab144e902a8e8e76215a2345b3860fe92ad28 ;; \
        amd64) platform="linux-x64-musl"; checksum=e6ff03823153a699d708c88c2fa87d18a1f6b45f13b14a985a221f8af442dd54 ;; \
        *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    wget -q "https://downloads.claude.ai/claude-code-releases/${cli_version}/${platform}/claude" -O claude; \
    echo "${checksum}  claude" | sha256sum -c -; \
    chmod 0755 claude

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS runtime
LABEL org.opencontainers.image.title="native-agent-api-claude" \
      org.opencontainers.image.description="native-agent-api with the native Claude Code CLI"
# bash: Claude Code's Bash tool accepts only bash or zsh, and Codex prefers bash over sh.
# git: both CLIs use it to find the repository root, diffs and history. Projects that need more
# (language toolchains) extend this image. Packages come from the pinned Alpine release branch;
# `apk upgrade` also brings the base image packages to its latest security fixes.
# hadolint ignore=DL3018
RUN apk upgrade --no-cache \
    && apk add --no-cache bash git \
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
