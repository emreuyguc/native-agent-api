# Security policy

native-agent-api runs a coding agent that can execute commands and change files, so its security
boundaries matter. Thank you for helping to keep them sound.

## Supported versions

Until 1.0.0, only the latest released minor version receives security fixes.

| Version | Supported |
|---|---|
| 0.2.x | Yes |
| 0.1.x | No |

## Reporting a vulnerability

Please do not open a public issue for a vulnerability.

Report it privately through GitHub:
[Report a vulnerability](https://github.com/emreuyguc/native-agent-api/security/advisories/new)
(repository **Security** tab → **Report a vulnerability**).

Include what you can of:

- the version (`native-agent-api version`), the agent and its CLI version, and the platform;
- whether the gateway ran on the host or in the Docker setup, and the relevant settings
  (never send real tokens or credentials);
- the steps to reproduce and what an attacker gains.

You can expect an acknowledgement within 7 days and an assessment within 14 days. Fixes are
released as soon as they are ready; the advisory is published with the fix, crediting you unless
you prefer otherwise.

## Scope

In scope are weaknesses in this project, for example:

- reaching the API without a valid token, from a disallowed IP, by DNS rebinding or from another
  browser origin;
- reading or writing files outside a workspace or an allowed root through the API;
- an agent run escaping the configured policy: the server environment, credentials or the gateway
  itself becoming reachable, agent-written configuration loading into a later run, or permission
  answers being bypassed;
- tokens, credentials or secrets appearing in logs, events or responses;
- weaknesses in the provided Docker setup.

Out of scope:

- vulnerabilities in Claude Code, Codex or other third-party software: report them to their
  maintainers (the [Anthropic](https://www.anthropic.com/responsible-disclosure-policy) and
  [OpenAI](https://openai.com/security/disclosure/) disclosure programs);
- what an agent does with the access it was deliberately given, such as `tools.permissions:
  bypass` in a workspace, or a host directory attached on purpose;
- deployments that disable protections explicitly, such as `NATIVE_AGENT_ALLOW_UNAUTHENTICATED=true`
  on a reachable listener.

The [security model](docs/security.md) describes the intended boundaries in detail.
