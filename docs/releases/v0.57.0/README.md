# Jerboa v0.57.0

Release notes and validation evidence.

- Desktop: `ac39d202a62f9f0a6ea60c2c4d18cf13c796edab` (unchanged since v0.56.0)
- Firecracker macOS: `d1fe2641a391e43672e9fa6c07b335f5a5eee07c` (unchanged since v0.56.0)
- Product: `v0.57.0`; Linux kernel toolset: `v0.2.3`, the published one, reused.

## What changes

**Signed images.**
- **Signing:** a push from GitHub Actions by a trusted publisher is signed keyless with [Sigstore](https://www.sigstore.dev/). It carries two attestations, its SLSA provenance (repository, commit, workflow, run) and its SBOM, logged in Rekor.
- **Verification on pull:** pulls verify every signed image. The signature is checked against `--identity`, then the policy's rule for the namespace, then the signer remembered for the repository, then the workflow the registry recorded as uploader.
- **Remembered signers:** the first verified pull remembers the signer. Later pulls refuse another signer or an unsigned image.
- **New commands:** `jerboa registry verify` checks an image without pulling it. `jerboa registry signers` lists the remembered signers, and `registry signers forget` drops one.

**Official images.** `jerboa/` holds images built and published by [AitorConS/jerboa-images](https://github.com/AitorConS/jerboa-images), starting with `jerboa/redis` and `jerboa/nginx`. A built-in policy rule accepts `jerboa/` images only when that repository's workflow on `main` signed them.

**Several platforms under one tag.**
- `jerboa push --index <target> <image>…` tags an image index of images already pushed, one per platform.
- `jerboa pull` picks this host's platform from an index, or the one `--platform` names.

**SBOMs.** Every build records a CycloneDX SBOM of the image's programs, packages, Go modules, npm packages, Python distributions and Cargo crates. `jerboa images sbom <ref>` prints it, and `jerboa build --sbom <file>` also writes it to a file.

**Publishing from CI without a token.** In a GitHub Actions job with `permissions: id-token: write`, `jerboa push` exchanges the job's OIDC token for a push token from a namespace's trusted publisher, with no stored credential. The `AitorConS/jerboa/.github/actions/setup-jerboa` action installs a pinned, verified release in a workflow.

**Deleting from the CLI.** `jerboa registry repo rm ns/repo:tag` deletes a tag. `jerboa registry repo rm ns/repo` deletes a repository with all its tags, after asking, unless `--yes` is given.

**Dependencies.** go-jose 4.1.6, which fixes a denial of service when parsing JWKs.

Packages are published separately from releases. The package index now has caddy 2.11.7, nginx 1.30.6 and redis 7.2.16.

## Upgrade notes

- **Pulls verify signed images.** An image that has signatures is kept only if they verify. If a repository's publisher changes on purpose, `jerboa registry signers forget <repo>` lets the next pull remember the new one. `--verify` also refuses unsigned images.
- **`jerboa/` images must be signed by the official workflow.** An unsigned image, or one signed by anyone else, under `jerboa/` is refused.
- **Pushes from GitHub Actions sign by default** when they push with the job's OIDC token. `--sign=false` skips signing.
- **Clients up to v0.56.0** pulling a tag published for several platforms get the `linux/amd64` image.
