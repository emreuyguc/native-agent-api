# syntax=docker/dockerfile:1.7

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

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS codex-download
ARG TARGETARCH
WORKDIR /download
SHELL ["/bin/ash", "-eo", "pipefail", "-c"]
# The Codex CLI release the adapter is tested against, with the SHA-256 of each platform's
# package. Update all three together with the version in internal/agent/codex.
RUN set -eu; \
    cli_version=0.167.0; \
    case "$TARGETARCH" in \
        arm64) platform="aarch64-unknown-linux-musl"; checksum=0de261ecf1e7f3028a7952f03fca2093f749277d38590b78bb32f8567756bbb7 ;; \
        amd64) platform="x86_64-unknown-linux-musl"; checksum=3bbf75e83de6b58aed28f2cf6e33ad423ec31fd4700a1457b94be557ea5a7c6e ;; \
        *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    archive="codex-package-${platform}.tar.gz"; \
    wget -q "https://github.com/openai/codex/releases/download/rust-v${cli_version}/${archive}" -O codex.tar.gz; \
    echo "${checksum}  codex.tar.gz" | sha256sum -c -; \
    mkdir package; \
    tar -xzf codex.tar.gz -C package; \
    test -x package/bin/codex

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
LABEL org.opencontainers.image.title="native-agent-api-codex" \
      org.opencontainers.image.description="native-agent-api with the native Codex CLI package"
# bash: Claude Code's Bash tool accepts only bash or zsh, and Codex prefers bash over sh.
# git: both CLIs use it to find the repository root, diffs and history. Projects that need more
# (language toolchains) extend this image. Packages come from the pinned Alpine release branch.
# hadolint ignore=DL3018
RUN apk add --no-cache bash git \
    && addgroup -S -g 65532 runner \
    && adduser -S -D -H -u 65532 -G runner -h /home/runner -s /bin/bash runner \
    && mkdir -p /home/runner/.codex /data /projects /tmp /opt/codex \
    && chown -R runner:runner /home/runner /data /projects /tmp \
    && chmod 0700 /home/runner /home/runner/.codex /data
COPY --from=gateway-build --chown=root:root /out/native-agent-api /usr/local/bin/native-agent-api
COPY --from=codex-download --chown=root:root /download/package/ /opt/codex/
USER 65532:65532
WORKDIR /data
ENV HOME=/home/runner \
    USER=runner \
    TMPDIR=/tmp \
    PATH=/opt/codex/bin:/opt/codex/codex-path:/usr/local/bin:/usr/bin:/bin
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/native-agent-api"]
CMD ["serve"]
