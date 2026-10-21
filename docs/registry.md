---
layout: default
title: Image registry
description: "Share unikernel images through registry.jerboa.dev: log in with GitHub, push, pull, and use it from CI."
nav_order: 12
---

# Image registry

`registry.jerboa.dev` stores Jerboa images so they can be run on any machine.

## How it works

The registry doesn't store disk images. It stores each image's **recipe** and the files it was built from:
- **The recipe** records the exact `mkfs` manifest, the kernel toolset version and the disk's digest.
- **On pull:**
  1. Your daemon fetches the files it doesn't have yet.
  2. It assembles the disk locally with that toolset's `mkfs`.
  3. It checks the result byte-for-byte against the published digest.

Because of this, pushes and pulls move only what changed:
- A new version of an app uploads and downloads only the files that differ.
- A runtime already used by any image on the registry (Node, Python…) is stored once and fetched once per machine.

## Log in

```sh
jerboa login
```

`login` shows a code to enter at `https://github.com/login/device`. Once you approve it, the registry links your GitHub account. You can push to:
- your user, as `<your-login>/<repository>`;
- any organization you belonged to when you logged in.

Pulling public images needs no login.

The credential is saved in `~/.jerboa/credentials.json` (mode 0600) and stays with the CLI. For each push, the daemon only receives a token that is valid for 15 minutes and that one repository.

## Push

Name the image after its repository, then push:

```sh
jerboa build . --name web
jerboa tag web:latest aitorcons/web:v1
jerboa push aitorcons/web:v1
```

Before uploading, the push rebuilds the disk from the recipe on your machine. That way it never publishes an image that others couldn't rebuild.

Images built before Jerboa recorded recipes have none and must be rebuilt before they can be pushed. `jerboa images inspect <image>` shows the recipe digest, and `jerboa images recipe <image>` prints the recipe itself.

## Pull and run

```sh
jerboa pull aitorcons/web:v1
jerboa run aitorcons/web:v1 -p 8080:8080
```

A reference is `[registry/]namespace/repository[:tag|@sha256:<digest>]`:
- The registry defaults to `registry.jerboa.dev`.
- The tag defaults to `latest`.
- A digest names an exact recipe and never changes.

## CI

Create a personal access token once, on a machine where you are logged in:

```sh
jerboa registry pat create github-actions
```

Store the printed token as a secret and expose it as `JERBOA_REGISTRY_TOKEN`:

```yaml
- run: jerboa push aitorcons/web:${{ github.sha }}
  env:
    JERBOA_REGISTRY_TOKEN: ${{ secrets.JERBOA_REGISTRY_TOKEN }}
```

`jerboa registry pat ls` lists your tokens; `jerboa registry pat rm <id>` revokes one.

## Description and README

Each repository can carry a one-line description and a Markdown README, shown on its page on the hub. Set them from the command line or from CI, with the same credentials as a push:

```sh
jerboa registry repo edit aitorcons/web --description "Static web server" --readme README.md
```

Either flag can be used alone. `--readme -` reads the README from standard input.

## Limits

| Limit | Value |
|---|---|
| Storage per namespace | 10 GiB, counting the files the namespace uploaded first |
| Largest file, compressed | 64 MiB |
| Files per image | 100 000 |
| Tags per repository | 10 000 |
| Description | 200 characters, one line |
| README | 64 KiB |

A recipe that no tag references is deleted after 7 days, along with any files no remaining recipe uses.

Images are public. Snapshots and volumes are not distributed through the registry.

The wire protocol is documented in [Registry protocol]({% link registry-protocol.md %}).
