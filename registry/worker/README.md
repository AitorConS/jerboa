# Jerboa registry Worker

The Cloudflare Worker behind `registry.jerboa.dev`. It authenticates users
through GitHub, issues push tokens, verifies every uploaded pack blob by blob,
and writes the static layout that pulls read from R2 through the CDN. Pulls
never reach it. The protocol is specified in
[`docs/registry-protocol.md`](../../docs/registry-protocol.md).

| Path | Contents |
|---|---|
| `src/index.ts` | Router, discovery, the daily garbage-collection cron |
| `src/auth.ts` | GitHub login, refresh tokens, personal access tokens, push JWTs |
| `src/push.ts` | Missing blobs, pack upload, recipes, tags |
| `src/pack.ts` | Pack verification (mirrors `internal/registry/pack`) |
| `src/gc.ts` | Garbage collection |
| `migrations/` | D1 schema |
| `test/` | Vitest suite on local R2 and D1, with a fake GitHub |

## Develop and test

Node 24 and pnpm 11:

```sh
pnpm install
pnpm exec tsc --noEmit
pnpm exec vitest run
```

`test/fixtures/go.json` holds a pack, a recipe and a hostile frame produced by
the Go client, so the suite checks both sides of the protocol against the same
bytes. Regenerate it after changing the formats:

```sh
JERBOA_WORKER_FIXTURE=$PWD/test/fixtures/go.json go test ../../internal/registry/recipe -run WorkerFixture
```

To run the Worker locally, with R2 and D1 kept in `.wrangler/` and the static
layout served under `/cdn/`:

```sh
echo 'JWT_SIGNING_KEY=local-development-signing-key-0123456789' > .dev.vars
pnpm exec wrangler d1 migrations apply jerboa-registry-local --local
pnpm exec wrangler dev --local --port 8788
```

## Deploy

The `Registry Worker` workflow deploys:
- **staging:** on every merge to `main`;
- **production:** from a manual, approved run.

Each environment (`staging`, `production`) has its own R2 bucket, D1 database, custom domain and GitHub OAuth App. All of them are managed by Terraform in `AitorConS/jerboa-infra`, which also sets the deploy job's GitHub environment. See [`docs/release-process.md`](../../docs/release-process.md).

To deploy by hand, export the same variables the job gets from its environment, then:

```sh
scripts/configure-env.sh staging secrets.json   # fills wrangler.toml; don't commit it
pnpm exec wrangler d1 migrations apply jerboa-registry-staging --env staging --remote
pnpm exec wrangler deploy --env staging --secrets-file secrets.json
rm secrets.json && git checkout wrangler.toml
```
