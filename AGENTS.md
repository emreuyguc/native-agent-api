# Agent instructions

Instructions for coding agents working in this repository. Claude Code and Codex CLI both read
this file. To add a repository skill, put it in `.agents/skills/<name>/` and add a symlink
`.claude/skills/<name>` → `../../.agents/skills/<name>` so Claude Code finds it too.

## Project rules

native-agent-api serves one native agent CLI (Claude Code or Codex CLI) over HTTP.

### Product

- **Native stays native.** Pass native behavior through; never re-implement an agent feature in the
  gateway.
- **No hidden defaults.** Every gateway setting is explicit and validated at startup. An omitted
  optional request field keeps the CLI's native behavior; never substitute a value of our own.
- **Common fields only when every agent has them** with the same meaning. Agent-only data goes in
  the typed `provider` section, next to the unmodified `native` document.
- **One process serves one agent.** Adapters live in `internal/agent/<name>`; nothing outside them
  branches on the agent.
- **The binary is the product.** Docker is an optional helper, not a run mode.

### Safety

- Never read, change or delete the user's native profile (`~/.claude`, `~/.claude.json`,
  `~/.codex`) or credentials; never log in or out of it.
- Never pass the server environment or host credentials to the CLI or into containers.
- Every change that can widen what the agent reaches needs a test and an entry in
  [docs/security.md](docs/security.md).

### Code and checks

- Run `make check` before every commit; it must pass clean.
- Never add AI attribution to commits or pull requests: no `Co-Authored-By` lines for an AI and no
  "Generated with" footers.
- The OpenAPI document is generated from Go types; never edit it by hand.
- A new adapter adds `docs/agents/<cli-name>.md`, `docker/<name>/` and a README row.
- Tests cover real behavior and security boundaries; do not test scenarios that cannot happen.
- Changes that need the real CLI are checked as described in [docs/testing.md](docs/testing.md).

## Writing rules

For documentation, comments, commit messages and API descriptions. Based on the Google and
Microsoft developer style guides and ASD-STE100 (Simplified Technical English).

- **Short sentences:** at most 20 words for an instruction, 25 for a description.
- **One idea per sentence, one topic per paragraph.**
- **Active voice, present tense.** "The gateway rejects the request", not "the request will be
  rejected".
- **Address the reader as "you"** in guides; use the imperative for steps.
- **Cut filler:** "in order to", "it should be noted that", "basically", "currently".
- **One term for one thing.** Do not alternate synonyms (run / execution / job).
- **Keep exact names:** fields, flags, commands, error codes and test names in backticks. Never
  shorten them away.
- **Say each fact once.** Link to the page that owns it instead of repeating it.
- **Prefer a table or list** when three or more items share a structure.
- **Shorter, not vaguer.** If cutting a word loses meaning, a reason or a keyword, keep it.
