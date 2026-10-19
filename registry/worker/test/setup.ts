import { applyD1Migrations, env } from "cloudflare:test";
import { beforeEach } from "vitest";

await applyD1Migrations(env.DB, env.TEST_MIGRATIONS);

// Every test starts from an empty registry.
beforeEach(async () => {
  for (const table of ["tags", "recipe_packs", "recipes", "blobs", "packs", "repos", "tokens", "accounts"]) {
    await env.DB.prepare(`DELETE FROM ${table}`).run();
  }
  let cursor: string | undefined;
  do {
    const page = await env.BLOBS.list({ cursor });
    if (page.objects.length) await env.BLOBS.delete(page.objects.map((o) => o.key));
    cursor = page.truncated ? page.cursor : undefined;
  } while (cursor);
});
