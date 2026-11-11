# Docker

The optional Docker setup runs native-agent-api in one hardened container per installation. One
installation serves one agent and has two files:

| File | Contents | Example |
|---|---|---|
| Docker env file | Compose settings, image, port, limits, the agent image and the path of the application env file. No application setting. | [`docker/.env.example`](.env.example) |
| Application env file | The `NATIVE_AGENT_*` settings, in the format of the host `.env` | [`/.env.example`](../.env.example) |

`docker/compose.yaml` defines the container. `docker/compose.egress.yaml` is an optional overlay
that restricts egress (see [Restricted egress](#restricted-egress)).

## Setup

Run every command from the repository root.

1. Create the Docker env file and the application env file:

   ```sh
   cp docker/.env.example docker/claude.env
   cp .env.example docker/claude.app.env
   chmod 600 docker/claude.env docker/claude.app.env
   ```

2. In `docker/claude.env`, set `CONTAINER_APP_ENV_FILE=./claude.app.env` and the `BUILD_*` values.
   The other values serve Claude on `127.0.0.1:8701`.
3. In `docker/claude.app.env`, set `NATIVE_AGENT_AGENT=claude` and the container values:

   | Setting | Value | Reason |
   |---|---|---|
   | `NATIVE_AGENT_SANDBOX_MODE` | `external` | The container is the isolation boundary; bubblewrap cannot create namespaces in it |
   | `NATIVE_AGENT_LISTEN` | `0.0.0.0:8080` | The published port reaches the container interface, never its loopback |
   | `NATIVE_AGENT_DATA_DIR` | `/data` | Named volume |
   | `NATIVE_AGENT_ALLOWED_ROOTS` | `/projects` | Named volume for attachable projects |
   | `NATIVE_AGENT_PROFILE_DIR` | `native` | `HOME` (`/home/runner`) is a named volume, so the native profile and its login persist |

4. Create a client token. Write the digest to `NATIVE_AGENT_TOKEN_SHA256` in
   `docker/claude.app.env`, and keep `$TOKEN` for the requests below:

   ```sh
   TOKEN=$(openssl rand -hex 32)
   printf %s "$TOKEN" | shasum -a 256 | cut -d' ' -f1
   ```

   `native-agent-api new-token` prints the same pair, on the host or with
   `docker compose --env-file docker/claude.env run --rm agent new-token`.

5. Start the container:

   ```sh
   docker compose --env-file docker/claude.env up -d --build --wait
   ```

6. Log in through the API. The login lives in the home volume, so you log in once per
   installation:

   ```sh
   U=http://127.0.0.1:8701
   api() { curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
   api -X POST $U/v1/agent/login/start -d '{"method":"claudeai"}' | jq .step
   ```

   | Agent | `method` | Next step |
   |---|---|---|
   | Claude | `claudeai` (subscription) or `console` (API billing) | Open `step.url` and sign in. Send the code the page shows: `api -X POST $U/v1/agent/login/code -d '{"code":"<code>"}'` |
   | Codex | `chatgpt_device_code` | Open `step.url` and enter `step.user_code` |

   `api $U/v1/agent/status | jq .logged_in` prints `true` when the login is done.

   The API reference at `http://127.0.0.1:8701/docs` runs the same requests. The operations are
   in the **login** group: "Start login", then "Submit login code" for Claude. Enter `$TOKEN` as the bearer token in the page's authentication field first.

Do not use the `chatgpt` method or `native-agent-api login` for Codex in a container: they wait
for a browser callback on localhost inside the container, which your browser cannot reach.

`CONTAINER_AGENT` selects the image and must equal `NATIVE_AGENT_AGENT`. If they differ, the
container reports unhealthy, because the image has no CLI for the configured agent.

## Several installations

Each Docker env file is a separate installation: its own Compose project, volumes, port and
application env file. To serve Claude and Codex side by side, create `docker/codex.env` with:

| Setting | Value |
|---|---|
| `COMPOSE_PROJECT_NAME` | `native-agent-api-codex` |
| `CONTAINER_AGENT` | `codex` |
| `CONTAINER_APP_ENV_FILE` | `./codex.app.env` |
| `CONTAINER_IMAGE` | `native-agent-api-codex:local` |
| `CONTAINER_PUBLISH_PORT` | `8702` |
| `CONTAINER_EGRESS_ALLOW_FILE` | `./codex/egress-allow.txt` |

In `docker/codex.app.env`, set `NATIVE_AGENT_AGENT=codex`. Then use `--env-file docker/codex.env`
in every command and port `8702` for the API. Two installations of the same agent, for
example with two accounts, need only different project names and ports.

## Operations

| Task | Command (add `--env-file docker/<name>.env`) |
|---|---|
| Stop; volumes are kept | `docker compose down` |
| Rebuild the image without cache | `docker compose build --no-cache` |
| Delete the installation, including its login | `docker compose down -v` |
| Open a shell in the container | `docker compose exec agent bash` |

Check an installation end to end (hardening, tools and, with the egress overlay, the allowlist):
`docker/verify.sh docker/claude.env`.

`NATIVE_AGENT_LOG_LEVEL=debug` in the application env file raises the gateway's logging.

The container policy is an unprivileged UID, a read-only root filesystem, no Linux capabilities,
`no-new-privileges`, a PID limit, a bounded tmpfs and a healthcheck.

## Data and permissions

The container runs as UID and GID 65532 (`runner`) and cannot change file ownership. Three steps
prepare its directories:

| Step | What it does |
|---|---|
| Image | Creates `/data`, `/projects` and `/home/runner`, owned by `runner`. `/data` and `/home/runner` get mode `0700`. |
| Named volume | When Docker mounts an empty named volume, it copies the owner and mode of the image directory. |
| Gateway start | Creates `NATIVE_AGENT_DATA_DIR/work` and missing parents with mode `0700`, and `workspaces/` on first use. It never changes existing owners or modes. |

The gateway stops at startup when it cannot create the data directory. A host path such as
`/Users/...` in `NATIVE_AGENT_DATA_DIR` fails with `read-only file system`, because the root
filesystem is read-only.

A bind mount (a host directory instead of a named volume) keeps the host owner and mode. Give it to
UID 65532 before the first start, or the gateway fails with `permission denied`:

```sh
sudo chown 65532:65532 ./data && chmod 700 ./data
```

## Restricted egress

By default the agent reaches any host, so web tools, MCP servers and package installs work. To
allow only the hosts in `CONTAINER_EGRESS_ALLOW_FILE`, set in the Docker env file:

```sh
COMPOSE_FILE=docker/compose.yaml:docker/compose.egress.yaml
```

The agent then sits on an internal network with no route out and no external DNS. It reaches
allowlisted hosts through a proxy (`HTTPS_PROXY`, which the gateway passes on to the CLI). A
forwarder publishes the API port. The gateway therefore sees every client as the forwarder:
restrict who can connect with `CONTAINER_PUBLISH_ADDR` and the host firewall, not
`NATIVE_AGENT_ALLOWED_IPS`. The `egress` service logs blocked requests.

## Tools in the image

The images contain the CLI, `bash` (Claude Code's Bash tool needs bash or zsh) and `git`. Projects
that need language toolchains extend an image:

```dockerfile
FROM native-agent-api-claude:local
USER root
RUN apk add --no-cache nodejs npm python3
USER 65532:65532
```

## Upgrade from 0.1.0

0.1.0 used one `docker/.env` for both agents, with `compose.<agent>.yaml` overlays and profiles.

1. Stop the old services with the 0.1.0 command, without `-v`.
2. Create one Docker env file per agent as above. Point `CONTAINER_APP_ENV_FILE` at the existing
   application env file.
3. Keep `COMPOSE_PROJECT_NAME=native-agent-api-claude` (or `-codex`): the volumes keep their
   names, so the login and the data stay.
