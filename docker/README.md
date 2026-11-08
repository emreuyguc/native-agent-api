# Docker

The optional Docker setup runs one native-agent-api adapter per hardened container.

Compose is split by responsibility:

- `compose.yaml` contains agent-neutral runtime policy.
- `compose.<agent>.yaml` contains the image, port, resources, credentials volume, workspaces, and
  project mounts for one agent.
- The application env file is supplied externally and contains explicit container paths and server
  settings.
- `docker/.env` contains only Compose values such as image, host port, CPU, memory, build identity,
  and the external application env file path.

Each agent gets its own complete application env file, so one process still serves one agent while
both services can run in the same Compose project.

## Setup

Claude:

```sh
cp docker/.env.example docker/.env
cp .env.example /absolute/path/to/native-agent-api.claude.env
# Edit both files, set the container paths from `docker/.env.example` and replace every example path and build-identity value.
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.claude.yaml --profile claude run --rm claude new-token
# Write the generated digest to NATIVE_AGENT_TOKEN_SHA256 in the application env file.
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.claude.yaml --profile claude run --rm claude login
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.claude.yaml --profile claude up -d --build
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.claude.yaml --profile claude logs -f claude
```

Codex uses the same flow with its own overlay and env file. Sign in with `chatgpt_device_code`: the
`chatgpt` method's callback listens on localhost inside the container.

```sh
cp .env.example /absolute/path/to/native-agent-api.codex.env
# Set NATIVE_AGENT_AGENT=codex and the same container paths.
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.codex.yaml --profile codex run --rm codex new-token
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.codex.yaml --profile codex run --rm codex login
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.codex.yaml --profile codex up -d --build
```

Both overlays can be combined to run Claude and Codex side by side, each with its own credentials,
workspaces, project volumes, port, resource limits and env file:

```sh
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.claude.yaml -f docker/compose.codex.yaml --profile claude --profile codex up -d --build
```

## Operations

Stop a service (named volumes are kept):

```sh
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.claude.yaml --profile claude down
```

Check a service end to end (hardening, tools and, with `--egress`, the allowlist):
`docker/verify.sh claude` or `docker/verify.sh codex --egress`.

The shared policy: an unprivileged UID, a read-only root filesystem, no Linux capabilities,
`no-new-privileges`, a PID limit, a bounded tmpfs and a healthcheck. Credentials, workspaces and
mounted projects are per agent.

A new agent uses the same layout, `compose.<agent>.yaml` and `<agent>/Dockerfile`, and adds only its
Compose variables to `docker/.env.example`; application settings stay external.

## Restricted egress

By default the agent reaches any host, so web tools, MCP servers and package installs work. Add
`compose.<agent>.egress.yaml` to allow only the hosts in `docker/<agent>/egress-allow.txt` (or the
file `<AGENT>_EGRESS_ALLOW_FILE` names):

```sh
docker compose --env-file docker/.env -f docker/compose.yaml -f docker/compose.claude.yaml -f docker/compose.claude.egress.yaml --profile claude up -d --build
```

The agent then sits on an internal network with no route out and no external DNS. It reaches
allowlisted hosts through a proxy (`HTTPS_PROXY`, which the gateway passes on to the CLI), and the
API port is forwarded to it. Every client therefore appears to the gateway as the forwarder:
restrict who can connect with `<AGENT>_HOST` and the host firewall, not `NATIVE_AGENT_ALLOWED_IPS`.
Blocked requests are logged by the `<agent>-egress` service.

## Tools in the image

The images contain the CLI, `bash` (Claude Code's Bash tool needs bash or zsh) and `git`. Projects
that need language toolchains extend an image:

```dockerfile
FROM native-agent-api-claude:local
USER root
RUN apk add --no-cache nodejs npm python3
USER 65532:65532
```

For debugging, `docker compose ... exec claude bash` opens a shell and `NATIVE_AGENT_LOG_LEVEL=debug`
in the application env file raises the gateway's logging.
