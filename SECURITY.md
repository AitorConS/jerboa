# Security Policy

## Supported Status

Jerboa is currently best treated as a public beta / experimental systems project.

That means:

- security issues should still be reported responsibly
- absence of a formal support SLA should not be read as indifference to real issues

## Reporting

If you find a security issue, prefer a private report before opening a public issue with exploit detail.

Include:

- affected commit or release
- platform
- reproduction steps
- impact
- any known workaround

Report it privately through GitHub's private vulnerability reporting for this
repository: <https://github.com/AitorConS/jerboa/security/advisories/new>.

## Verifying a Download

Releases are published at `https://releases.jerboa.dev`. Each release has a
manifest, `releases/vX.Y.Z/manifest.json`, signed with
[minisign](https://jedisct1.github.io/minisign/) by the release key. The
public key is embedded in the installer (`PUBLIC_KEY` in `scripts/install.sh`)
and in the CLI (`PublicKeyB64` in `internal/release/config.go`):

```
RWQUeMEQrLXFcshAMUevjf6nlhsSB1PuZYt5dFhb0za9aypwSAUMorsH
```

To check a file by hand, here the Linux CLI of v0.60.0:

```bash
v=v0.60.0
base=https://releases.jerboa.dev
curl -fsSLO "$base/releases/$v/manifest.json"
curl -fsSLO "$base/releases/$v/manifest.json.minisig"
minisign -Vm manifest.json -P RWQUeMEQrLXFcshAMUevjf6nlhsSB1PuZYt5dFhb0za9aypwSAUMorsH
jq -r .channel manifest.json        # must print: stable
curl -fsSLO "$base/cli/$v/jerboa-linux-amd64"
jq -r '.components.cli.platforms["linux-amd64"].sha256' manifest.json
sha256sum jerboa-linux-amd64        # the same digest (macOS: shasum -a 256)
```

`minisign` must report a valid signature with the trusted comment
`release:vX.Y.Z` for the version you asked for, and the channel must be
`stable`: release candidates are signed with the same key for the
`candidate` channel and are not releases. Every other file (`daemon`,
`desktop`, `distro`, `kernel`) is listed in the same manifest with its URL and
SHA-256. `install.sh` and the `jerboa` CLI make these checks themselves when
they download a release.

## Scope Hints

High-priority reports include:

- daemon authentication bypass
- unauthorized control over the VM lifecycle API
- host/network privilege escalation paths
- unsafe WSL/Windows trust-boundary issues
- signing/verification bypasses

## Current Reality

Because the project is still maturing:

- some surfaces are inherently higher-risk
- platform differences matter
- public-beta framing should stay visible

Do not assume hard multitenant guarantees unless they are explicitly stated and verified.
