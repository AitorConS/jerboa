import { nowSeconds, requirePush } from "./auth";
import type { Env } from "./env";
import { invalid, RegistryError, tooLarge } from "./errors";
import { checkRepo, checkTag, digestFromHex, hexOf, isDigest } from "./names";
import { decodeUpload, MAX_PACK } from "./pack";
import { checkRecipe, MAX_RECIPE, MEDIA_TYPE } from "./recipe";

const IMMUTABLE = "public, max-age=31536000, immutable";
const TAG_CACHE = "public, max-age=60";
const MAX_MISSING_QUERY = 10_000;
const MAX_TAGS = 10_000;

async function body(req: Request, limit: number): Promise<Uint8Array> {
  const declared = Number(req.headers.get("Content-Length") ?? "0");
  if (declared > limit) throw tooLarge(`body is over ${limit} bytes`);
  const data = new Uint8Array(await req.arrayBuffer());
  if (data.length > limit) throw tooLarge(`body is over ${limit} bytes`);
  return data;
}

/** POST /v2/<ns>/<repo>/blobs/missing */
export async function missingBlobs(req: Request, env: Env, ns: string, repo: string): Promise<Response> {
  checkRepo(ns, repo);
  await requirePush(req, env, ns, repo);
  const b = (await req.json().catch(() => null)) as { digests?: unknown } | null;
  const digests = b?.digests;
  if (!Array.isArray(digests) || digests.length > MAX_MISSING_QUERY || !digests.every(isDigest)) {
    throw invalid("BAD_REQUEST", `digests must list up to ${MAX_MISSING_QUERY} digests`);
  }
  const { results } = await env.DB.prepare(
    `SELECT digest FROM blobs WHERE digest IN (SELECT value FROM json_each(?1))`,
  )
    .bind(JSON.stringify(digests))
    .all<{ digest: string }>();
  const known = new Set(results.map((r) => r.digest));
  return Response.json({ missing: [...new Set(digests as string[])].filter((d) => !known.has(d)) });
}

/** PUT /v2/<ns>/<repo>/packs/<hex> */
export async function putPack(req: Request, env: Env, ns: string, repo: string, hex: string): Promise<Response> {
  checkRepo(ns, repo);
  const digest = digestFromHex(hex, "PACK_INVALID");
  await requirePush(req, env, ns, repo);
  if (await env.DB.prepare(`SELECT 1 FROM packs WHERE digest = ?1`).bind(digest).first()) {
    await req.body?.cancel();
    return Response.json({ pack: digest, registered: 0 });
  }
  const pack = await decodeUpload(await body(req, MAX_PACK + (16 << 20) + 4));
  if (pack.digest !== digest) throw invalid("PACK_INVALID", "pack digest does not match the URL");
  const used = await env.DB.prepare(`SELECT COALESCE(SUM(size), 0) AS used FROM packs WHERE namespace = ?1`)
    .bind(ns)
    .first<{ used: number }>();
  if ((used?.used ?? 0) + pack.size > Number(env.NAMESPACE_QUOTA_BYTES)) {
    throw new RegistryError(403, "QUOTA", `namespace ${ns} is over its storage quota`);
  }
  // The object goes first and the rows after it, in one transaction: a blob
  // is never visible before the pack that holds it. An object left behind by
  // a failed transaction is removed by garbage collection.
  await env.BLOBS.put(`packs/${hex}`, pack.data, {
    httpMetadata: { contentType: "application/octet-stream", cacheControl: IMMUTABLE },
  });
  const now = nowSeconds();
  const [, inserted] = await env.DB.batch([
    env.DB.prepare(`INSERT OR IGNORE INTO packs (digest, size, namespace, created_at) VALUES (?1, ?2, ?3, ?4)`).bind(
      digest,
      pack.size,
      ns,
      now,
    ),
    // A blob keeps the location of the first pack that carried it.
    env.DB.prepare(
      `INSERT OR IGNORE INTO blobs (digest, pack, offset, length, size)
       SELECT json_extract(value, '$.digest'), ?1, json_extract(value, '$.offset'),
              json_extract(value, '$.length'), json_extract(value, '$.size')
       FROM json_each(?2)`,
    ).bind(digest, JSON.stringify(pack.blobs)),
  ]);
  return Response.json({ pack: digest, registered: inserted?.meta.changes ?? 0 }, { status: 201 });
}

/** PUT /v2/<ns>/<repo>/recipes/<hex> */
export async function putRecipe(req: Request, env: Env, ns: string, repo: string, hex: string): Promise<Response> {
  checkRepo(ns, repo);
  const digest = digestFromHex(hex, "RECIPE_INVALID");
  await requirePush(req, env, ns, repo);
  const bytes = await body(req, MAX_RECIPE);
  if (await env.DB.prepare(`SELECT 1 FROM recipes WHERE digest = ?1`).bind(digest).first()) {
    return Response.json({ recipe: digest });
  }
  const { blobs, summary } = await checkRecipe(bytes, digest);
  const { results } = await env.DB.prepare(
    `SELECT digest, pack, offset, length FROM blobs WHERE digest IN (SELECT json_extract(value, '$.digest') FROM json_each(?1))`,
  )
    .bind(JSON.stringify(blobs))
    .all<{ digest: string; pack: string; offset: number; length: number }>();
  if (results.length !== blobs.length) {
    const found = new Set(results.map((r) => r.digest));
    const missing = blobs.find((b) => !found.has(b.digest))!;
    throw new RegistryError(409, "BLOB_UNKNOWN", `blob ${missing.digest} has not been uploaded`);
  }
  results.sort((a, b) => (a.digest < b.digest ? -1 : 1));
  const locations: Record<string, { pack: string; offset: number; length: number }> = {};
  for (const r of results) locations[r.digest] = { pack: r.pack, offset: r.offset, length: r.length };
  const packs = [...new Set(results.map((r) => r.pack))];
  await env.BLOBS.put(`locations/${hex}.json`, JSON.stringify({ blobs: locations }), {
    httpMetadata: { contentType: "application/json", cacheControl: IMMUTABLE },
  });
  await env.BLOBS.put(`recipes/${hex}.json`, bytes, {
    httpMetadata: { contentType: MEDIA_TYPE, cacheControl: IMMUTABLE },
  });
  const now = nowSeconds();
  await env.DB.batch([
    env.DB.prepare(
      `INSERT OR IGNORE INTO recipes (digest, size, created_at, untagged_since, summary) VALUES (?1, ?2, ?3, ?3, ?4)`,
    ).bind(digest, bytes.length, now, JSON.stringify(summary)),
    env.DB.prepare(`INSERT OR IGNORE INTO recipe_packs (recipe, pack) SELECT ?1, value FROM json_each(?2)`).bind(
      digest,
      JSON.stringify(packs),
    ),
  ]);
  return Response.json({ recipe: digest }, { status: 201 });
}

async function repoID(env: Env, ns: string, repo: string, create: boolean): Promise<number | null> {
  if (create) {
    await env.DB.prepare(`INSERT OR IGNORE INTO repos (namespace, name, created_at) VALUES (?1, ?2, ?3)`)
      .bind(ns, repo, nowSeconds())
      .run();
  }
  const row = await env.DB.prepare(`SELECT id FROM repos WHERE namespace = ?1 AND name = ?2`)
    .bind(ns, repo)
    .first<{ id: number }>();
  return row?.id ?? null;
}

const tagKey = (ns: string, repo: string, tag: string) => `tags/${ns}/${repo}/${tag}`;

/** PUT /v2/<ns>/<repo>/tags/<tag> */
export async function putTag(req: Request, env: Env, ns: string, repo: string, tag: string): Promise<Response> {
  checkRepo(ns, repo);
  checkTag(tag);
  await requirePush(req, env, ns, repo);
  const b = (await req.json().catch(() => null)) as { recipe?: unknown } | null;
  if (!isDigest(b?.recipe)) throw invalid("BAD_REQUEST", "recipe digest is required");
  const recipe = b.recipe;
  if (!(await env.DB.prepare(`SELECT 1 FROM recipes WHERE digest = ?1`).bind(recipe).first())) {
    throw new RegistryError(409, "RECIPE_UNKNOWN", `recipe ${recipe} has not been uploaded`);
  }
  const id = (await repoID(env, ns, repo, true))!;
  const old = await env.DB.prepare(`SELECT recipe FROM tags WHERE repo_id = ?1 AND tag = ?2`)
    .bind(id, tag)
    .first<{ recipe: string }>();
  if (!old) {
    const count = await env.DB.prepare(`SELECT COUNT(*) AS n FROM tags WHERE repo_id = ?1`).bind(id).first<{ n: number }>();
    if ((count?.n ?? 0) >= MAX_TAGS) throw tooLarge(`repository has ${MAX_TAGS} tags`);
  }
  const now = nowSeconds();
  const stmts = [
    env.DB.prepare(
      `INSERT INTO tags (repo_id, tag, recipe, updated_at) VALUES (?1, ?2, ?3, ?4)
       ON CONFLICT (repo_id, tag) DO UPDATE SET recipe = ?3, updated_at = ?4`,
    ).bind(id, tag, recipe, now),
    env.DB.prepare(`UPDATE recipes SET untagged_since = NULL WHERE digest = ?1`).bind(recipe),
    touchRepo(env, id, now),
  ];
  if (old && old.recipe !== recipe) stmts.push(markUntagged(env, old.recipe, now));
  await env.DB.batch(stmts);
  await env.BLOBS.put(tagKey(ns, repo, tag), JSON.stringify({ recipe }), {
    httpMetadata: { contentType: "application/json", cacheControl: TAG_CACHE },
  });
  return Response.json({ tag, recipe });
}

const touchRepo = (env: Env, id: number, now: number) =>
  env.DB.prepare(`UPDATE repos SET updated_at = ?2 WHERE id = ?1`).bind(id, now);

function markUntagged(env: Env, recipe: string, now: number): D1PreparedStatement {
  return env.DB.prepare(
    `UPDATE recipes SET untagged_since = ?2 WHERE digest = ?1 AND NOT EXISTS (SELECT 1 FROM tags WHERE recipe = ?1)`,
  ).bind(recipe, now);
}

/** DELETE /v2/<ns>/<repo>/tags/<tag> */
export async function deleteTag(req: Request, env: Env, ns: string, repo: string, tag: string): Promise<Response> {
  checkRepo(ns, repo);
  checkTag(tag);
  await requirePush(req, env, ns, repo);
  await removeTag(env, ns, repo, tag);
  return new Response(null, { status: 204 });
}

/** Deletes a tag; its recipe becomes garbage once no tag uses it. The caller checks access. */
export async function removeTag(env: Env, ns: string, repo: string, tag: string): Promise<void> {
  const id = await repoID(env, ns, repo, false);
  const old = id
    ? await env.DB.prepare(`SELECT recipe FROM tags WHERE repo_id = ?1 AND tag = ?2`).bind(id, tag).first<{ recipe: string }>()
    : null;
  if (!old) throw new RegistryError(404, "NOT_FOUND", `${ns}/${repo}:${tag} does not exist`);
  const now = nowSeconds();
  await env.DB.batch([
    env.DB.prepare(`DELETE FROM tags WHERE repo_id = ?1 AND tag = ?2`).bind(id, tag),
    markUntagged(env, old.recipe, now),
    touchRepo(env, id!, now),
  ]);
  await env.BLOBS.delete(tagKey(ns, repo, tag));
}

/** GET /v2/<ns>/<repo>/tags (public) */
export async function listTags(env: Env, ns: string, repo: string): Promise<Response> {
  checkRepo(ns, repo);
  const { results } = await env.DB.prepare(
    `SELECT t.tag, t.recipe, t.updated_at AS updated FROM tags t JOIN repos r ON r.id = t.repo_id
     WHERE r.namespace = ?1 AND r.name = ?2 ORDER BY t.tag`,
  )
    .bind(ns, repo)
    .all();
  return Response.json({ tags: results });
}

export { hexOf };
