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

If no dedicated security contact has been published yet, use the project owner's private contact path rather than opening with full public exploit detail.

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
