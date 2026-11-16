---
layout: default
title: Changelog
description: "Where to find what changed in each jerboa version, and how the changelog relates to the per-release validation records."
nav_order: 17
---

# Changelog

What changed in each version of Jerboa is recorded in one file at the root of
the engine repository:

**[CHANGELOG.md](https://github.com/AitorConS/jerboa/blob/main/CHANGELOG.md)**

Each version starts with the changes that can affect an existing setup and
what to do about them, followed by what was added, deprecated, removed, fixed
and hardened. The `Unreleased` section at the top lists what is merged and
will ship in the next version.

Check which version you run, and whether a newer one is published:

```sh
jerboa version
```

## Changelog and release records

- **The changelog** says what changed for someone who uses Jerboa. The release
  notes of a version are its changelog section.
- **[`docs/releases/`](https://github.com/AitorConS/jerboa/tree/main/docs/releases)**
  holds one record per release with its validation evidence: the candidate
  run, the commits it was built from, file hashes, and what was and was not
  tested. Records up to v0.59.0 also carry that release's notes; later ones
  link to the changelog instead of repeating them.
