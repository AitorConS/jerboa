import type { Env } from "./env";
import { invalid, RegistryError } from "./errors";
import { checkNamespace, checkRepo, checkTag, hexOf } from "./names";
import { parseRecipe, summarize, type RecipeSummary } from "./recipe";

// The public catalog the hub reads: search, namespaces, repositories and
// tags. Every answer is public and may be a minute stale.

const CATALOG_CACHE = { "Cache-Control": "public, max-age=60" };
export const PAGE_SIZE = 20;
const MAX_QUERY = 100;

export interface RepoCard {
  namespace: string;
  name: string;
  description: string;
  pulls: number;
  updated: number;
}

export interface RepoDetail extends RepoCard {
  readme: string;
  created: number;
  tags: number;
}

export interface TagDetail {
  tag: string;
  recipe: string;
  size: number;
  updated: number;
  summary: RecipeSummary | null;
}

const notFound = (what: string) => new RegistryError(404, "NOT_FOUND", `${what} does not exist`);

const CARD = `r.namespace, r.name, r.description, r.pulls, r.updated_at AS updated`;
// Repositories whose tags were all deleted stay out of listings.
const LISTED = `EXISTS (SELECT 1 FROM tags t WHERE t.repo_id = r.id)`;

/** Escapes LIKE wildcards so a query matches literally. */
const likePattern = (q: string) => `%${q.replace(/[\\%_]/g, (c) => "\\" + c)}%`;

/**
 * Repositories matching q in their name or description, most pulled first,
 * or most recently updated first when sort is "updated". An empty q lists
 * every repository. One extra row tells whether another page follows.
 */
export async function searchRepos(
  env: Env,
  q: string,
  page: number,
  sort: "pulls" | "updated" = "pulls",
): Promise<{ repos: RepoCard[]; next: boolean }> {
  q = q.trim().toLowerCase();
  if (q.length > MAX_QUERY) throw invalid("BAD_REQUEST", `query is over ${MAX_QUERY} characters`);
  if (!Number.isSafeInteger(page) || page < 1 || page > 500) throw invalid("BAD_REQUEST", "invalid page");
  const order = sort === "updated" ? "r.updated_at DESC" : "r.pulls DESC, r.updated_at DESC";
  const { results } = await env.DB.prepare(
    `SELECT ${CARD} FROM repos r
     WHERE ${LISTED} AND (?1 = '' OR (r.namespace || '/' || r.name) LIKE ?2 ESCAPE '\\' OR lower(r.description) LIKE ?2 ESCAPE '\\')
     ORDER BY ${order}, r.id LIMIT ?3 OFFSET ?4`,
  )
    .bind(q, likePattern(q), PAGE_SIZE + 1, (page - 1) * PAGE_SIZE)
    .all<RepoCard>();
  return { repos: results.slice(0, PAGE_SIZE), next: results.length > PAGE_SIZE };
}

/** A namespace's repositories, most recently updated first, or NOT_FOUND when it has none. */
export async function namespaceRepos(env: Env, ns: string): Promise<RepoCard[]> {
  checkNamespace(ns);
  const { results } = await env.DB.prepare(
    `SELECT ${CARD} FROM repos r WHERE r.namespace = ?1 AND ${LISTED} ORDER BY r.updated_at DESC LIMIT 1000`,
  )
    .bind(ns)
    .all<RepoCard>();
  if (!results.length) throw notFound(`namespace ${ns}`);
  return results;
}

export async function repoDetail(env: Env, ns: string, repo: string): Promise<RepoDetail> {
  checkRepo(ns, repo);
  const row = await env.DB.prepare(
    `SELECT ${CARD}, r.readme, r.created_at AS created, (SELECT COUNT(*) FROM tags t WHERE t.repo_id = r.id) AS tags
     FROM repos r WHERE r.namespace = ?1 AND r.name = ?2`,
  )
    .bind(ns, repo)
    .first<RepoDetail>();
  if (!row) throw notFound(`${ns}/${repo}`);
  return row;
}

export async function tagDetail(env: Env, ns: string, repo: string, tag: string): Promise<TagDetail> {
  checkRepo(ns, repo);
  checkTag(tag);
  const row = await env.DB.prepare(
    `SELECT t.tag, t.recipe, t.updated_at AS updated, c.size, c.summary
     FROM tags t JOIN repos r ON r.id = t.repo_id JOIN recipes c ON c.digest = t.recipe
     WHERE r.namespace = ?1 AND r.name = ?2 AND t.tag = ?3`,
  )
    .bind(ns, repo, tag)
    .first<Omit<TagDetail, "summary"> & { summary: string | null }>();
  if (!row) throw notFound(`${ns}/${repo}:${tag}`);
  const summary = row.summary ? (JSON.parse(row.summary) as RecipeSummary) : await backfillSummary(env, row.recipe);
  return { ...row, summary };
}

/** Summarizes a recipe pushed before summaries existed, and stores the result. */
async function backfillSummary(env: Env, digest: string): Promise<RecipeSummary | null> {
  const obj = await env.BLOBS.get(`recipes/${hexOf(digest)}.json`);
  if (!obj) return null;
  let summary: RecipeSummary;
  try {
    summary = summarize(parseRecipe(new Uint8Array(await obj.arrayBuffer())));
  } catch {
    return null;
  }
  await env.DB.prepare(`UPDATE recipes SET summary = ?2 WHERE digest = ?1`).bind(digest, JSON.stringify(summary)).run();
  return summary;
}

/** GET /v2/_search, /v2/<ns>, /v2/<ns>/<repo> and /v2/<ns>/<repo>/tags/<tag>; parts follow /v2. */
export async function catalog(url: URL, env: Env, parts: string[]): Promise<Response> {
  const json = (body: unknown) => Response.json(body, { headers: CATALOG_CACHE });
  const [ns, repo, kind, tag] = parts;
  if (ns === "_search" && parts.length === 1) {
    const sort = url.searchParams.get("sort") === "updated" ? "updated" : "pulls";
    return json(await searchRepos(env, url.searchParams.get("q") ?? "", Number(url.searchParams.get("page") ?? "1"), sort));
  }
  if (ns && parts.length === 1) return json({ namespace: ns, repos: await namespaceRepos(env, ns) });
  if (ns && repo && parts.length === 2) return json(await repoDetail(env, ns, repo));
  if (ns && repo && kind === "tags" && tag && parts.length === 4) return json(await tagDetail(env, ns, repo, tag));
  throw new RegistryError(404, "NOT_FOUND", "no such endpoint");
}
