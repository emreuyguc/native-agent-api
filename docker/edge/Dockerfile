# syntax=docker/dockerfile:1.7
#
# Network edge for the restricted egress overlay (compose.egress.yaml): tinyproxy allows
# outbound HTTP(S) only to the domains in the mounted allowlist, socat forwards the published API
# port to the agent, which itself has no route out.

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
# Packages come from the pinned Alpine release branch;
# `apk upgrade` also brings the base image packages to its latest security fixes.
# hadolint ignore=DL3018
RUN apk upgrade --no-cache \
    && apk add --no-cache tinyproxy socat
COPY docker/edge/tinyproxy.conf /etc/tinyproxy/tinyproxy.conf
USER 65532:65532
