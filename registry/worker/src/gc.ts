import type { Env } from "./env";

const GRACE = 7 * 24 * 3600;
const ORPHAN_AGE_MS = 24 * 3600 * 1000;

/**
 * Daily garbage collection: recipes no tag has used for 7 days, then packs no
 * remaining recipe uses for 7 days, with their blob registrations, and pack
 * objects a failed upload left without database rows.
 */
export async function collectGarbage(env: Env, now: number): Promise<{ recipes: number; packs: number; orphans: number }> {
  // Recipes: start the clock on untagged ones, collect expired ones.
  await env.DB.prepare(
    `UPDATE recipes SET untagged_since = ?1
     WHERE untagged_since IS NULL AND NOT EXISTS (SELECT 1 FROM tags WHERE tags.recipe = recipes.digest)`,
  )
    .bind(now)
    .run();
  const { results: oldRecipes } = await env.DB.prepare(
    `SELECT digest FROM recipes WHERE untagged_since IS NOT NULL AND untagged_since < ?1
     AND NOT EXISTS (SELECT 1 FROM tags WHERE tags.recipe = recipes.digest) LIMIT 500`,
  )
    .bind(now - GRACE)
    .all<{ digest: string }>();
  for (const { digest } of oldRecipes) {
    const hex = digest.slice(7);
    await env.BLOBS.delete([`recipes/${hex}.json`, `locations/${hex}.json`]);
    await env.DB.batch([
      env.DB.prepare(`DELETE FROM recipe_packs WHERE recipe = ?1`).bind(digest),
      env.DB.prepare(`DELETE FROM recipes WHERE digest = ?1`).bind(digest),
    ]);
  }

  // Packs: same clock for packs no recipe uses.
  await env.DB.batch([
    env.DB.prepare(
      `UPDATE packs SET unused_since = ?1
       WHERE unused_since IS NULL AND NOT EXISTS (SELECT 1 FROM recipe_packs WHERE recipe_packs.pack = packs.digest)`,
    ).bind(now),
    env.DB.prepare(
      `UPDATE packs SET unused_since = NULL
       WHERE unused_since IS NOT NULL AND EXISTS (SELECT 1 FROM recipe_packs WHERE recipe_packs.pack = packs.digest)`,
    ),
  ]);
  const { results: oldPacks } = await env.DB.prepare(
    `SELECT digest FROM packs WHERE unused_since IS NOT NULL AND unused_since < ?1
     AND NOT EXISTS (SELECT 1 FROM recipe_packs WHERE recipe_packs.pack = packs.digest) LIMIT 500`,
  )
    .bind(now - GRACE)
    .all<{ digest: string }>();
  for (const { digest } of oldPacks) {
    await env.DB.batch([
      env.DB.prepare(`DELETE FROM blobs WHERE pack = ?1`).bind(digest),
      env.DB.prepare(`DELETE FROM packs WHERE digest = ?1`).bind(digest),
    ]);
    await env.BLOBS.delete(`packs/${digest.slice(7)}`);
  }

  // Pack objects without rows, from uploads whose transaction failed.
  let orphans = 0;
  const listed = await env.BLOBS.list({ prefix: "packs/", limit: 1000 });
  for (const obj of listed.objects) {
    if (Date.now() - obj.uploaded.getTime() < ORPHAN_AGE_MS) continue;
    const digest = "sha256:" + obj.key.slice("packs/".length);
    if (!(await env.DB.prepare(`SELECT 1 FROM packs WHERE digest = ?1`).bind(digest).first())) {
      await env.BLOBS.delete(obj.key);
      orphans++;
    }
  }
  return { recipes: oldRecipes.length, packs: oldPacks.length, orphans };
}
