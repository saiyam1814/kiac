# Releasing kiac

Application releases are built only from SemVer tags on `main`. The release workflow:

1. Runs the full source CI suite.
2. Builds the darwin/arm64 archive with GoReleaser.
3. Generates an SPDX SBOM and SHA-256 checksums.
4. Uploads everything to a draft GitHub release.
5. Creates GitHub/Sigstore provenance and SBOM attestations.
6. Updates the Homebrew tap with its repository-scoped deploy key.
7. Publishes the release only after every preceding step succeeds.

Published releases and their tags are immutable. If a draft release fails partway through, fix the cause and rerun the failed workflow before publishing it. Never replace an artifact under an existing version; create a new patch release.

## One-time repository setup

The `HOMEBREW_TAP_DEPLOY_KEY` Actions secret contains an unencrypted SSH private key. Its public half must be a write-enabled deploy key on `saiyam1814/homebrew-tap`. It has no access to any other repository.

## Create a release

Start from a clean, current `main`, choose the next SemVer version, and push an annotated tag:

```bash
git switch main
git pull --ff-only
make ci
git tag -a vX.Y.Z -m "kiac vX.Y.Z"
git push origin vX.Y.Z
```

After the workflow succeeds, verify both the artifact provenance and GitHub's immutable release attestation:

```bash
gh release download vX.Y.Z --repo saiyam1814/kiac --dir kiac-release
(cd kiac-release && shasum -a 256 -c checksums.txt)
gh attestation verify kiac-release/kiac_X.Y.Z_darwin_arm64.tar.gz --repo saiyam1814/kiac
gh release verify vX.Y.Z --repo saiyam1814/kiac
```

Kernel releases use `kernel-v*` tags and follow the same draft-first publication rule. After publishing a new kernel, update its SHA-256 pin in `pkg/cluster/kernel.go` through a normal pull request.
