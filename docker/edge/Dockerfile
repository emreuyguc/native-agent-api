# syntax=docker/dockerfile:1.7
#
# Network edge for the restricted egress overlay (compose.egress.yaml): tinyproxy allows
# outbound HTTP(S) only to the domains in the mounted allowlist, socat forwards the published API
# port to the agent, which itself has no route out.

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
# Packages come from the pinned Alpine release branch;
# `apk upgrade` also brings the base image packages to its latest security fixes.
# hadolint ignore=DL3018
RUN apk upgrade --no-cache \
    && apk add --no-cache tinyproxy socat
COPY docker/edge/tinyproxy.conf /etc/tinyproxy/tinyproxy.conf
USER 65532:65532
