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

This document is the contract between the registry server and the Jerboa
daemon. Go types live in
`internal/registry/{recipe,pack,cas}`.

## Hosts

| Host | Serves |
|---|---|
| `registry.jerboa.dev` | Authentication and every write |
| `cdn.registry.jerboa.dev` | Every read during a pull, as static files |

A pull reads only from the CDN. `GET https://registry.jerboa.dev/v2/`
returns the discovery document (cacheable for 5 minutes):

```json
{"api": 1, "cdn": "https://cdn.registry.jerboa.dev", "login": "device", "features": ["oidc", "index"]}
```

`features` lists optional parts of the protocol the registry supports:
- `"oidc"` is [GitHub Actions OIDC](#github-actions-oidc-trusted-publishers);
- `"index"` is [image indexes](#image-indexes).

`"login": "device"` means the registry runs the GitHub sign-in itself (see
[Login](#login-jerboa-login)). Older registries advertise
`"github_client_id"` instead.

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
  `recipe.Recipe`). Its digest is the SHA-256 of exactly those bytes, which the
  registry serves unchanged.
- **Pack:** a concatenation of independent zstd frames, one per blob, in the order
  of its index. A pack has no header; it is identified by the SHA-256 of its
  bytes. A pack holds at most 64 MiB (v1), so the largest blob must compress to
  64 MiB or less.
- **Locations:** for one recipe, where each of its blobs lives:
  ```json
  {"blobs": {"sha256:…": {"pack": "sha256:…", "offset": 0, "length": 1234}}}
  ```
  Locations are a performance hint, not trusted: the client verifies every blob.
- **Index:** canonical JSON (`application/vnd.jerboa.image.index.v1+json`, see
  `imageindex.Index`) listing one recipe per platform:
  ```json
  {"schemaVersion":1,"mediaType":"application/vnd.jerboa.image.index.v1+json","manifests":[{"digest":"sha256:…","platform":"linux/amd64"},{"digest":"sha256:…","platform":"linux/arm64"}]}
  ```
  Compact, fields in this order, 1 to 8 entries sorted by platform and one per
  platform, so one set of recipes has one digest.
- **Tag document:** `{"recipe": "sha256:…"}`, or `{"recipe": "sha256:…", "index": "sha256:…"}` for a tag naming an index; `recipe` is then the index's default recipe.

## Static layout (CDN)

| Key | Content | Cache-Control |
|---|---|---|
| `packs/<hex>` | Pack bytes; served with range support | `public, max-age=31536000, immutable` |
| `recipes/<hex>.json` | Recipe bytes | `public, max-age=31536000, immutable` |
| `locations/<hex>.json` | Locations of that recipe | `public, max-age=31536000, immutable` |
| `indexes/<hex>.json` | Index bytes | `public, max-age=31536000, immutable` |
| `tags/<ns>/<repo>/<tag>` | Tag document | `public, max-age=60` |

`<hex>` is the digest without its `sha256:` prefix. A deleted tag answers 404.

## Pull

1. `GET {cdn}/tags/<ns>/<repo>/<tag>`. A reference by digest skips this step. If the tag document names an `index`: `GET {cdn}/indexes/<hex>.json`, verify its SHA-256, parse it (`imageindex.Parse`), and pull the recipe of the platform asked for (`--platform`), or of the host's (`linux/<arch>`); a client that does not know indexes pulls `recipe`.
2. `GET {cdn}/recipes/<hex>.json`. Verify the SHA-256 of the bytes against the digest, then parse it (`recipe.Parse`, which rejects non-canonical encodings).
3. Drop the blobs already in the local store. If none are missing, go to step 6.
4. `GET {cdn}/locations/<hex>.json`. Group the missing blobs by pack and merge frames less than 64 KiB apart (`pack.PlanRanges`).
5. For each range: `GET {cdn}/packs/<hex>` with `Range: bytes=<start>-<end>`. Decompress each frame, bounded to the blob's size from the recipe; verify the SHA-256 (`pack.ReadBlob`); store it.
6. Obtain the recipe's `mkfs` (`tools.MkfsForVersion`), assemble the disk (`recipe.Assemble`), verify `diskDigest`, and store the image.

7. `POST https://registry.jerboa.dev/v2/<ns>/<repo>/pulls`, anonymous and without a body, reports the pull for the hub's counter. The server answers 204, also for unknown repositories. The client sends it once, waits at most 2 seconds and ignores the result.

Retries:
- A failed range is retried with backoff.
- A blob that fails verification aborts the pull. It is never stored.

## Authentication

The model follows Docker's: interactive login, personal access tokens for CI,
short-lived scoped tokens for each write, anonymous reads. GitHub is the identity
provider.

### Login (`jerboa login`)

The registry runs the [GitHub device flow](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow) with the `read:org` scope; the GitHub token never reaches the client.

1. `POST /auth/device` (no body) answers
   `{"login_id": "jdl_…", "user_code": "ABCD-1234", "verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": 5}`.
   The client shows the code and the registry it is for; the user enters it at `verification_uri` and approves.
2. The client polls `POST /auth/device/token` with `{"login_id": "jdl_…"}`, waiting `interval` seconds before each poll, until `expires_in` has passed:
   - `202 {"status": "pending", "interval": 5}`: not answered yet. The client waits the `interval` given, which grows if it polls too fast.
   - `200 {"token": "jrt_…", "login": "aitorcons"}`: signed in. A login id signs in once.
   - `403`: the user denied it. `410`: the code expired. `404`: unknown or already used login id. The client stops and reports it.
3. The `jrt_` refresh token lasts 90 days and can be revoked.

**Older registries** advertise `github_client_id` instead of `"login": "device"`. With them the client runs the device flow itself with that app and posts the GitHub token to `POST /auth/github` with `{"access_token": "<github token>"}`, which answers as in step 2.
- A registry accepts any GitHub token issued to its app. So the client refuses to log in to another registry that advertises the default registry's `github_client_id`, and refuses when it cannot check. Such a registry can still be used with `--token`.
- Registries that run the flow themselves answer `POST /auth/github` with `410`.

### Personal access tokens (CI)

Authenticated with a refresh token or a PAT:
- `POST /auth/pats {"name": "ci"}` → `{"id": "…", "token": "jpat_…"}`. The token is shown once.
- `GET /auth/pats` lists them.
- `DELETE /auth/pats/<id>` revokes one.

### Scoped tokens

`POST /auth/token` with `Authorization: Bearer <jrt_ or jpat_>` and a body such as
`{"scopes": ["repository:aitorcons/web:push"]}` returns `{"token": "<token>", "expires_in": 900}`. Clients treat the token as opaque.

### GitHub Actions OIDC (trusted publishers)

`POST /auth/oidc` with no `Authorization` and a body
`{"token": "<GitHub Actions OIDC token>", "scopes": ["repository:aitorcons/web:push"]}`
returns `{"token": "<token>", "expires_in": 900}`, as `/auth/token` does.

- The client requests the token from GitHub Actions (`ACTIONS_ID_TOKEN_REQUEST_URL` with `audience=<registry host>`, authorized by `ACTIONS_ID_TOKEN_REQUEST_TOKEN`; the job needs `permissions: id-token: write`).
- The registry verifies GitHub's signature, the issuer, the audience (its own host) and the token's times. It grants the scopes only if one trusted publisher of the namespace allows them all: the GitHub repository and owner by id, the workflow file, and optionally the ref, the environment, the event, one repository, and `delete`.
- `403`: no trusted publisher matches, or the run was triggered by a pull request. `401`: the token is invalid, expired, or for another audience.
- Removing the trusted publisher ends the tokens issued for it at once.

A namespace is granted only if it is the account's login, or an organization the account belonged to the last time it logged in. Every write below needs `Authorization: Bearer <token>` with the matching `push` scope.

## Push

1. **Missing blobs:** `POST /v2/<ns>/<repo>/blobs/missing` with `{"digests": [...]}` (at most 10 000 per call) → `{"missing": [...]}`.
2. **Packs:** for each pack of missing blobs, `PUT /v2/<ns>/<repo>/packs/<hex>` with `Content-Type: application/vnd.jerboa.pack.v1`.
   - Body (`pack.EncodeUpload`): a 4-byte big-endian index length, the JSON index `{"digest","size","blobs":[{"digest","size","offset","length"}]}`, then the pack bytes.
   - The server rejects the upload unless it passes `pack.DecodeUpload`: the pack digest and size, frames that tile the pack in order, and every blob decompressed to its declared size and digest.
   - A blob already stored keeps its existing location.
   - Re-uploading a stored pack returns 200 with nothing changed.
3. **Recipe:** `PUT /v2/<ns>/<repo>/recipes/<hex>` with the recipe bytes.
   - The server checks:
     - the SHA-256 against the URL;
     - the schema, media type, and sorted and unique blobs;
     - the size limits;
     - that every listed blob is registered.
   - It publishes `recipes/<hex>.json` and `locations/<hex>.json`.
   - Idempotent.
4. **Tag:** `PUT /v2/<ns>/<repo>/tags/<tag>` with `{"recipe": "sha256:…"}`.
   - The recipe must exist.
   - The server publishes `tags/<ns>/<repo>/<tag>`.

Other tag operations:
- `DELETE /v2/<ns>/<repo>/tags/<tag>` removes a tag.
- `GET /v2/<ns>/<repo>/tags` → `{"tags": [{"tag","recipe","index","updated"}]}`; `index` is null for a tag naming one recipe.

### Image indexes

A registry that advertises `"index"` stores indexes, so one tag serves every
platform an image is built for:

1. Push each platform's image as usual, under a tag or by its recipe.
2. `PUT /v2/<ns>/<repo>/indexes/<hex>` with the index bytes (`Content-Type: application/vnd.jerboa.image.index.v1+json`, at most 16 KiB).
   - The server checks the SHA-256 against the URL and the canonical form (`INDEX_INVALID`), and that every recipe is registered (`RECIPE_UNKNOWN`) and is for the platform it is listed under (`INDEX_INVALID`).
   - It publishes `indexes/<hex>.json`. Idempotent.
3. `PUT /v2/<ns>/<repo>/tags/<tag>` with `{"index": "sha256:…"}` (`INDEX_UNKNOWN` if it is not registered).
   - The tag's `recipe` becomes the index's default: the `linux/amd64` one, or the first. Clients that know nothing of indexes pull it.
   - The repository holds every recipe of the index, so each can be pulled by digest.

`jerboa push --index <target> <image>…` does steps 2 and 3 from images
already pushed, reading each one's platform from its recipe. Indexes are
pulled by tag; a reference by digest names a recipe.

Before uploading anything, the client assembles the image from the recipe and its local blob store and checks the disk digest, so it never publishes a recipe that does not rebuild its disk.

## Repository details

`PATCH /v2/<ns>/<repo>` sets what the hub shows about a repository. It needs a push token for the repository and takes `{"description": "…", "readme": "…"}`; either field may be left out.
- **`description`:** whitespace collapses to single spaces; at most 200 characters.
- **`readme`:** Markdown, at most 64 KiB.

It answers 404 for a repository nothing was pushed to.

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
| `INDEX_INVALID` | 400 | Index digest, schema, form or platforms are wrong |
| `BLOB_UNKNOWN` | 409 | A recipe names a blob the registry does not have |
| `RECIPE_UNKNOWN` | 409 | A tag or index points at a recipe the registry does not have |
| `INDEX_UNKNOWN` | 409 | A tag points at an index the registry does not have |
| `TOO_LARGE` | 413 | Over a size limit |
| `QUOTA` | 403 | The namespace's storage quota is used up |
| `RATE_LIMITED` | 429 | Too many requests; honor `Retry-After` |

## Limits (v1)

| Limit | Value |
|---|---|
| Pack size, and largest compressed blob | 64 MiB |
| Blobs per recipe | 100 000 |
| Recipe size | 16 MiB |
| Index size, and platforms per index | 16 KiB, 8 |
| Digests per `blobs/missing` call | 10 000 |
| Tags per repository | 10 000 |
| Storage per namespace | 10 GiB (counts the packs the namespace uploaded first) |

## Garbage collection

Indexes and recipes that no tag has referenced for 7 days are deleted (a tag
naming an index references all its recipes, and an index keeps its recipes
while it lasts), recipes with their locations, and so are packs that no
remaining recipe uses. A blob whose pack
was deleted is reported missing again, so the next push that needs it uploads
it afresh.

## Versioning

The discovery document's `api` field is the protocol version. A server keeps
serving older versions while their clients are supported; a client refuses an
`api` it does not know.
