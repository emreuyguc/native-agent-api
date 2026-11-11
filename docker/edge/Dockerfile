# syntax=docker/dockerfile:1.7
#
# Network edge for the restricted egress overlay (compose.egress.yaml): tinyproxy allows
# outbound HTTP(S) only to the domains in the mounted allowlist, socat forwards the published API
# port to the agent, which itself has no route out.

ARG RUNTIME_IMAGE
FROM ${RUNTIME_IMAGE}
# Packages come from the pinned Alpine release branch of RUNTIME_IMAGE.
# hadolint ignore=DL3018
RUN apk add --no-cache tinyproxy socat
COPY docker/edge/tinyproxy.conf /etc/tinyproxy/tinyproxy.conf
USER 65532:65532
