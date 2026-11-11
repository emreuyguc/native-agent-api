# Releasing

A version tag starts [`.github/workflows/release.yml`](../.github/workflows/release.yml). It runs
the checks of `ci.yml`, then publishes:

| Output | Where | Contents |
|---|---|---|
| Binaries | GitHub release of the tag | `tar.gz` per platform (linux and darwin, amd64 and arm64), an SBOM per archive, `checksums.txt` |
| Images | `ghcr.io/emreuyguc/native-agent-api-claude`, `-codex`, `-edge` | amd64 and arm64, tags `<version>` and `<major>.<minor>`, SBOM and provenance from BuildKit |
| Attestations | GitHub attestations of the repository | Signed build provenance of every archive and image |

The release notes are the tag's section of [CHANGELOG.md](../CHANGELOG.md).
[GoReleaser](https://goreleaser.com) builds the binaries from [`.goreleaser.yaml`](../.goreleaser.yaml)
with the same identity as `make build`.

## Release a version

1. In `CHANGELOG.md`, rename `[Unreleased]` to `[0.2.0] - <date>`, add an empty `[Unreleased]`
   section and update the links at the end.
2. In `SECURITY.md`, update the supported version.
3. Merge the change into `main` through a pull request.
4. Check that the notes are complete: `scripts/release-notes.sh v0.2.0`.
5. Tag the merge commit on `main` and push the tag:

   ```sh
   git tag -a v0.2.0 -m v0.2.0 && git push origin v0.2.0
   ```

The workflow fails before it publishes anything when the checks fail or `CHANGELOG.md` has no
section for the tag. After the first release, set each GHCR package to public: package page,
**Package settings**, **Change visibility**.

## Verify a release

The attestations are signed through Sigstore without a key. The
[GitHub CLI](https://cli.github.com) verifies that a file or image comes from this repository's
release workflow:

```sh
gh attestation verify native-agent-api_0.2.0_linux_amd64.tar.gz --repo emreuyguc/native-agent-api
gh attestation verify oci://ghcr.io/emreuyguc/native-agent-api-claude:0.2.0 --repo emreuyguc/native-agent-api
```

To check only the download, compare it with `checksums.txt`:

```sh
sha256sum --ignore-missing -c checksums.txt
```
