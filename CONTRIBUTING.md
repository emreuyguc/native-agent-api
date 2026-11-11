# Contributing

Thank you for helping. This page covers what a pull request needs. The project rules and the
writing rules are in [AGENTS.md](AGENTS.md); they apply to people and coding agents alike.

## Before you start

- Report a vulnerability privately, never in an issue: see [SECURITY.md](SECURITY.md).
- For a larger change, open an issue first, so we agree on the approach before you write code.
- Read the [security model](docs/security.md) if the change touches the API, the CLI process,
  files or the Docker setup.

## Make the change

1. Set up the tools: Go (version in `go.mod`) and Docker. The other tools come with the
   repository; see [development](docs/development.md).
2. Keep native behavior native: pass the CLI's behavior through, do not re-implement it.
3. Add tests for real behavior. A change that can widen what the agent reaches also needs an entry
   in [docs/security.md](docs/security.md).
4. Update the documentation that owns each fact, and add a line to `CHANGELOG.md` under
   `[Unreleased]` for a change users notice.
5. Run `make check-pre-push` (or `make check-pre-commit` without Docker). It must pass clean.
   `make install-git-hooks` makes git run them for you. For a change to the Docker setup or the
   release, also run `make check-all`.

Changes to an adapter, the run lifecycle or the Docker setup also need the checks with the real
CLI in [testing](docs/testing.md); say in the pull request which ones you ran.

## Pull requests

- One topic per pull request. Write the commit subject in the imperative, for example
  "Reject empty tool rules".
- CI runs the same `make` targets, plus a dependency review and CodeQL, which need GitHub.
- A change that breaks API clients fails the `api` check. If it is intended, a maintainer adds the
  `api-breaking` label; see [API compatibility](docs/development.md#api-compatibility).
- Do not edit the OpenAPI document by hand: it is generated from the Go types.

By contributing, you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
