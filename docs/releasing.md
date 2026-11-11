# Releasing

A version tag starts [`.github/workflows/release.yml`](../.github/workflows/release.yml). It runs
the checks of `ci.yml`, builds every part, verifies it, and only then publishes:

| Output | Where | Contents |
|---|---|---|
| Binaries | GitHub release of the tag | `tar.gz` per platform (linux and darwin, amd64 and arm64), an SBOM per archive, `checksums.txt` |
| Images | `ghcr.io/emreuyguc/native-agent-api-claude`, `-codex`, `-edge` | amd64 and arm64, tags `<version>` and `<major>.<minor>` (no `latest`), SBOM and provenance from BuildKit |
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

The workflow runs these jobs in order. Nothing is public until `verify` passes.

| Job | What it does |
|---|---|
| `prepare` | Fails unless the tag is `vX.Y.Z` or `vX.Y.Z-rc.N`, its commit is on `main` and `CHANGELOG.md` has a section for it |
| `check` | Runs every job of `ci.yml` |
| `binaries` | Creates the GitHub release as a draft with the archives, SBOMs and `checksums.txt`, and attests the archives |
| `images` | Pushes each image by digest only, without a tag, and attests it |
| `verify` | Checks `checksums.txt`, the attestation of every archive and image, and the two image platforms. Then it adds the image tags and publishes the release |

A failed run leaves a draft release and untagged images. Delete the draft, fix the cause and push
a new tag. Releases are immutable after publishing, so a published version never changes.

After the first release, set each GHCR package to public: package page, **Package settings**,
**Change visibility**.

### Test the workflow with a release candidate

A `vX.Y.Z-rc.N` tag runs the same jobs. It uses the `CHANGELOG.md` section of `X.Y.Z`, becomes a
pre-release and moves no `<major>.<minor>` image tag. Tag the release commit with
`v0.3.0-rc.1` first, then with `v0.3.0` when the candidate is good.

## Verify a release

The attestations are signed through Sigstore without a key. The
[GitHub CLI](https://cli.github.com) verifies that a file or image comes from this repository's
release workflow. `--signer-workflow` and `--source-ref` also check the workflow and the tag:

```sh
gh attestation verify native-agent-api_0.2.0_linux_amd64.tar.gz --repo emreuyguc/native-agent-api \
  --signer-workflow emreuyguc/native-agent-api/.github/workflows/release.yml --source-ref refs/tags/v0.2.0
gh attestation verify oci://ghcr.io/emreuyguc/native-agent-api-claude:0.2.0 --repo emreuyguc/native-agent-api \
  --signer-workflow emreuyguc/native-agent-api/.github/workflows/release.yml --source-ref refs/tags/v0.2.0
```

To check only the download, compare it with `checksums.txt`:

```sh
sha256sum --ignore-missing -c checksums.txt
```
