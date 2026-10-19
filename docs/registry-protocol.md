---
layout: default
title: Registry protocol
description: "Wire protocol of the Jerboa image registry: recipes, blobs, packs, authentication, push and pull."
nav_order: 12
---

# Registry protocol (v1)

The Jerboa registry stores **image recipes** and the **blobs** they name, not
disk images. A client pulls a recipe, fetches the blobs it lacks, and rebuilds
the disk locally with the recipe's pinned `mkfs`. mkfs is byte-identical across
hosts for one toolset version (enforced by the `mkfs determinism` CI workflow),
so the result is exactly the published disk, which the client verifies.

This document is the contract between the registry server (a Cloudflare Worker
over R2 and D1) and the Jerboa daemon. Go types live in
`internal/registry/{recipe,pack,cas}`.

## Hosts

| Host | Serves | Through |
|---|---|---|
| `registry.jerboa.dev` | Authentication and every write | The Worker |
| `cdn.registry.jerboa.dev` | Every read during a pull | R2 behind the Cloudflare CDN |

A pull never reaches the Worker or the database. `GET https://registry.jerboa.dev/v2/`
returns the discovery document (cacheable for 5 minutes):

```json
{"api": 1, "cdn": "https://cdn.registry.jerboa.dev", "github_client_id": "Iv1.0123456789abcdef"}
```

## Names

| Item | Grammar |
|---|---|
| Namespace | `[a-z0-9](?:[a-z0-9-]{0,38})` — a GitHub user or organization login, lowercased |
| Repository | `[a-z0-9]+(?:[._-][a-z0-9]+)*`, at most 128 characters |
| Tag | `[A-Za-z0-9_][A-Za-z0-9._-]{0,127}` |
| Reference | `[registry/]namespace/repository[:tag|@sha256:<hex>]`; the registry defaults to `registry.jerboa.dev` and the tag to `latest` |

## Objects

Every digest is `sha256:<64 lowercase hex>`.

- **Blob:** the bytes of one file an image is built from, identified by the SHA-256 of its **uncompressed** bytes.
- **Recipe:** canonical JSON (`application/vnd.jerboa.image.recipe.v1+json`, see
  `recipe.Recipe`). Its digest is the SHA-256 of exactly those bytes. A server
  stores recipe bytes as received and never re-encodes them.
- **Pack:** a concatenation of independent zstd frames, one per blob, in the order
  of its index. A pack has no header; it is identified by the SHA-256 of its
  bytes. A pack holds at most 64 MiB (v1), so the largest blob must compress to
  64 MiB or less.
- **Locations:** for one recipe, where each of its blobs lives:
  ```json
  {"blobs": {"sha256:…": {"pack": "sha256:…", "offset": 0, "length": 1234}}}
  ```
  Locations are a performance hint, not trusted: the client verifies every blob.
- **Tag document:** `{"recipe": "sha256:…"}`.

## Static layout (CDN)

| Key | Content | Cache-Control |
|---|---|---|
| `packs/<hex>` | Pack bytes; served with range support | `public, max-age=31536000, immutable` |
| `recipes/<hex>.json` | Recipe bytes | `public, max-age=31536000, immutable` |
| `locations/<hex>.json` | Locations of that recipe | `public, max-age=31536000, immutable` |
| `tags/<ns>/<repo>/<tag>` | Tag document | `public, max-age=60` |

`<hex>` is the digest without its `sha256:` prefix. A deleted tag answers 404.

## Pull

1. `GET {cdn}/tags/<ns>/<repo>/<tag>`. A reference by digest skips this step.
2. `GET {cdn}/recipes/<hex>.json`. Verify the SHA-256 of the bytes against the digest, then parse it (`recipe.Parse`, which rejects non-canonical encodings).
3. Drop the blobs already in the local store. If none are missing, go to step 6.
4. `GET {cdn}/locations/<hex>.json`. Group the missing blobs by pack and merge frames less than 64 KiB apart (`pack.PlanRanges`).
5. For each range: `GET {cdn}/packs/<hex>` with `Range: bytes=<start>-<end>`. Decompress each frame, bounded to the blob's size from the recipe; verify the SHA-256 (`pack.ReadBlob`); store it.
6. Obtain the recipe's `mkfs` (`tools.MkfsForVersion`), assemble the disk (`recipe.Assemble`), verify `diskDigest`, and store the image.

Retries:
- A failed range is retried with backoff.
- A blob that fails verification aborts the pull. It is never stored.

## Authentication

The model follows Docker's: interactive login, personal access tokens for CI,
short-lived scoped tokens for each write, anonymous reads. GitHub is the identity
provider.

### Login (`jerboa login`)

1. The client runs the [GitHub device flow](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow) with `github_client_id` from discovery and the `read:org` scope. The user approves in a browser.
2. `POST /auth/github` with `{"access_token": "<github token>"}`.
   - The Worker reads the user and their organizations from the GitHub API, creates or updates the account, and discards the GitHub token.
   - It answers `{"token": "jrt_…", "login": "aitorcons"}`.
   - The `jrt_` refresh token lasts 90 days, can be revoked, and is stored only as a SHA-256 hash.

### Personal access tokens (CI)

Authenticated with a refresh token or a PAT:
- `POST /auth/pats {"name": "ci"}` → `{"id": "…", "token": "jpat_…"}`. The token is shown once.
- `GET /auth/pats` lists them.
- `DELETE /auth/pats/<id>` revokes one.

### Scoped tokens

`POST /auth/token` with `Authorization: Bearer <jrt_ or jpat_>` and a body such as
`{"scopes": ["repository:aitorcons/web:push"]}` returns `{"token": "<jwt>", "expires_in": 900}`.

The JWT is HS256 with claims:
- `sub`: account id
- `login`
- `scopes`
- `exp`

A namespace is granted only if it is the account's login, or an organization the account belonged to the last time it logged in. Every write below needs `Authorization: Bearer <jwt>` with the matching `push` scope.

## Push

1. **Missing blobs:** `POST /v2/<ns>/<repo>/blobs/missing` with `{"digests": [...]}` (at most 10 000 per call) → `{"missing": [...]}`.
2. **Packs:** for each pack of missing blobs, `PUT /v2/<ns>/<repo>/packs/<hex>` with `Content-Type: application/vnd.jerboa.pack.v1`.
   - Body (`pack.EncodeUpload`): a 4-byte big-endian index length, the JSON index `{"digest","size","blobs":[{"digest","size","offset","length"}]}`, then the pack bytes.
   - The server verifies the pack completely before anything becomes visible (`pack.DecodeUpload` semantics):
     - the pack digest and size;
     - frames that tile the pack in order;
     - every blob decompressed to its declared size and checked against its digest.
   - Without this check, a client could register a digest with other bytes and poison deduplication for everyone.
   - It then stores the pack and registers each blob that has no location yet. A blob keeps its first location for ever.
   - Re-uploading a stored pack returns 200 with nothing changed.
3. **Recipe:** `PUT /v2/<ns>/<repo>/recipes/<hex>` with the recipe bytes.
   - The server checks:
     - the SHA-256 against the URL;
     - the schema, media type, and sorted and unique blobs;
     - the size limits;
     - that every listed blob is registered.
   - It writes `recipes/<hex>.json` and `locations/<hex>.json`, and records which packs the recipe uses.
   - Idempotent.
4. **Tag:** `PUT /v2/<ns>/<repo>/tags/<tag>` with `{"recipe": "sha256:…"}`.
   - The recipe must exist.
   - The server updates the database, then `tags/<ns>/<repo>/<tag>`.

Other tag operations:
- `DELETE /v2/<ns>/<repo>/tags/<tag>` removes a tag.
- `GET /v2/<ns>/<repo>/tags` → `{"tags": [{"tag","recipe","updated"}]}`.

Before uploading anything, the client assembles the image from the recipe and its local blob store and checks the disk digest, so it never publishes a recipe that does not rebuild its disk.

## Errors

Errors are JSON with an HTTP status:

```json
{"error": {"code": "BLOB_INVALID", "message": "blob sha256:… does not match its digest"}}
```

| Code | Status | Meaning |
|---|---|---|
| `UNAUTHORIZED` | 401 | Missing, expired or revoked token |
| `DENIED` | 403 | Scope does not cover the namespace or action |
| `NAME_INVALID` | 400 | Namespace, repository or tag violates the grammar |
| `PACK_INVALID` | 400 | Pack digest, size or index is wrong |
| `BLOB_INVALID` | 400 | A blob does not decompress to its declared size or digest |
| `RECIPE_INVALID` | 400 | Recipe digest, schema or limits are wrong |
| `BLOB_UNKNOWN` | 409 | A recipe names a blob the registry does not have |
| `RECIPE_UNKNOWN` | 409 | A tag points at a recipe the registry does not have |
| `TOO_LARGE` | 413 | Over a size limit |
| `QUOTA` | 403 | The namespace's storage quota is used up |
| `RATE_LIMITED` | 429 | Too many requests; honor `Retry-After` |

## Limits (v1)

| Limit | Value |
|---|---|
| Pack size, and largest compressed blob | 64 MiB |
| Blobs per recipe | 100 000 |
| Recipe size | 16 MiB |
| Digests per `blobs/missing` call | 10 000 |
| Tags per repository | 10 000 |
| Storage per namespace | 10 GiB (counts the packs the namespace uploaded first) |

## Garbage collection

A daily job:
1. Deletes recipes that no tag has referenced for 7 days.
2. Deletes packs that no remaining recipe uses for 7 days, with their blob registrations.

Locations of deleted recipes go with them. A blob whose pack was deleted becomes
missing again and is uploaded afresh by the next push that needs it.

## Versioning

The discovery document's `api` field is the protocol version. A server keeps
serving older versions while their clients are supported; a client refuses an
`api` it does not know.
