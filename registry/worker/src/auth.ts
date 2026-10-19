import { randomToken, sha256Hex, signJWT, verifyJWT } from "./crypto";
import type { Env } from "./env";
import { denied, invalid, RegistryError, unauthorized } from "./errors";
import { checkRepo } from "./names";

const REFRESH_TTL = 90 * 24 * 3600;
const PUSH_TTL = 15 * 60;
const ISSUER = "registry.jerboa.dev";

export interface Account {
  id: number;
  login: string;
  orgs: string[];
}

export const nowSeconds = () => Math.floor(Date.now() / 1000);

async function github<T>(env: Env, path: string, token: string): Promise<T> {
  const res = await fetch(env.GITHUB_API + path, {
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: "application/vnd.github+json",
      "User-Agent": "jerboa-registry",
    },
  });
  if (res.status === 401 || res.status === 403) throw unauthorized("GitHub rejected the token");
  if (!res.ok) throw new RegistryError(502, "UPSTREAM", `GitHub answered ${res.status}`);
  return (await res.json()) as T;
}

/** POST /auth/github: exchanges a GitHub token for a registry refresh token. */
export async function loginWithGitHub(req: Request, env: Env): Promise<Response> {
  const body = (await req.json().catch(() => null)) as { access_token?: unknown } | null;
  if (!body || typeof body.access_token !== "string" || body.access_token === "") {
    throw invalid("BAD_REQUEST", "access_token is required");
  }
  const user = await github<{ id: number; login: string }>(env, "/user", body.access_token);
  const orgs = await github<{ login: string }[]>(env, "/user/orgs?per_page=100", body.access_token);
  const login = user.login.toLowerCase();
  const orgLogins = JSON.stringify(orgs.map((o) => o.login.toLowerCase()).sort());
  const now = nowSeconds();
  // A GitHub id keeps its account across renames; the login follows it.
  const account = await env.DB.prepare(
    `INSERT INTO accounts (github_id, login, orgs, created_at, updated_at) VALUES (?1, ?2, ?3, ?4, ?4)
     ON CONFLICT (github_id) DO UPDATE SET login = ?2, orgs = ?3, updated_at = ?4
     RETURNING id`,
  )
    .bind(user.id, login, orgLogins, now)
    .first<{ id: number }>();
  if (!account) throw new RegistryError(500, "INTERNAL", "account not stored");
  const token = randomToken("jrt_");
  await env.DB.prepare(
    `INSERT INTO tokens (id, account_id, kind, hash, created_at, expires_at) VALUES (?1, ?2, 'refresh', ?3, ?4, ?5)`,
  )
    .bind(crypto.randomUUID(), account.id, await sha256Hex(new TextEncoder().encode(token)), now, now + REFRESH_TTL)
    .run();
  return Response.json({ token, login });
}

/** Resolves a refresh token or PAT from the Authorization header. */
export async function accountFromBearer(req: Request, env: Env): Promise<Account> {
  const token = bearer(req);
  if (!token || !(token.startsWith("jrt_") || token.startsWith("jpat_"))) throw unauthorized();
  const now = nowSeconds();
  const row = await env.DB.prepare(
    `SELECT a.id, a.login, a.orgs, t.id AS token_id FROM tokens t JOIN accounts a ON a.id = t.account_id
     WHERE t.hash = ?1 AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > ?2)`,
  )
    .bind(await sha256Hex(new TextEncoder().encode(token)), now)
    .first<{ id: number; login: string; orgs: string; token_id: string }>();
  if (!row) throw unauthorized("token is unknown, expired or revoked");
  await env.DB.prepare(`UPDATE tokens SET last_used_at = ?1 WHERE id = ?2`).bind(now, row.token_id).run();
  return { id: row.id, login: row.login, orgs: JSON.parse(row.orgs) as string[] };
}

function bearer(req: Request): string | null {
  const h = req.headers.get("Authorization") ?? "";
  return h.startsWith("Bearer ") ? h.slice(7).trim() : null;
}

const SCOPE = /^repository:([^/:]+)\/([^/:]+):push$/;

function canPush(account: Account, namespace: string): boolean {
  return namespace === account.login || account.orgs.includes(namespace);
}

/** POST /auth/token: a short-lived JWT for the requested push scopes. */
export async function issuePushToken(req: Request, env: Env): Promise<Response> {
  const account = await accountFromBearer(req, env);
  const body = (await req.json().catch(() => null)) as { scopes?: unknown } | null;
  const scopes = body?.scopes;
  if (!Array.isArray(scopes) || scopes.length === 0 || scopes.length > 10) {
    throw invalid("BAD_REQUEST", "scopes must list 1 to 10 scopes");
  }
  for (const s of scopes) {
    const m = typeof s === "string" ? SCOPE.exec(s) : null;
    if (!m) throw invalid("BAD_REQUEST", `unsupported scope ${JSON.stringify(s)}`);
    checkRepo(m[1]!, m[2]!);
    if (!canPush(account, m[1]!)) throw denied(`${account.login} cannot push to ${m[1]}`);
  }
  const iat = nowSeconds();
  const token = await signJWT(
    { sub: account.id, login: account.login, scopes: scopes as string[], iat, exp: iat + PUSH_TTL, iss: ISSUER },
    env.JWT_SIGNING_KEY,
  );
  return Response.json({ token, expires_in: PUSH_TTL });
}

/** Requires a push token whose scopes cover namespace/repo. */
export async function requirePush(req: Request, env: Env, namespace: string, repo: string): Promise<string> {
  const token = bearer(req);
  if (!token) throw unauthorized();
  const claims = await verifyJWT(token, env.JWT_SIGNING_KEY, nowSeconds());
  if (!claims) throw unauthorized("push token is invalid or expired");
  if (!claims.scopes.includes(`repository:${namespace}/${repo}:push`)) {
    throw denied(`token does not grant push to ${namespace}/${repo}`);
  }
  return claims.login;
}

/** /auth/pats: personal access tokens for CI. */
export async function pats(req: Request, env: Env, id?: string): Promise<Response> {
  const account = await accountFromBearer(req, env);
  const now = nowSeconds();
  if (req.method === "POST" && !id) {
    const body = (await req.json().catch(() => null)) as { name?: unknown } | null;
    const name = typeof body?.name === "string" ? body.name.slice(0, 64) : "";
    if (!name) throw invalid("BAD_REQUEST", "name is required");
    const token = randomToken("jpat_");
    const tokenId = crypto.randomUUID();
    await env.DB.prepare(
      `INSERT INTO tokens (id, account_id, kind, name, hash, created_at) VALUES (?1, ?2, 'pat', ?3, ?4, ?5)`,
    )
      .bind(tokenId, account.id, name, await sha256Hex(new TextEncoder().encode(token)), now)
      .run();
    return Response.json({ id: tokenId, name, token }, { status: 201 });
  }
  if (req.method === "GET" && !id) {
    const { results } = await env.DB.prepare(
      `SELECT id, name, created_at, last_used_at FROM tokens
       WHERE account_id = ?1 AND kind = 'pat' AND revoked_at IS NULL ORDER BY created_at`,
    )
      .bind(account.id)
      .all();
    return Response.json({ pats: results });
  }
  if (req.method === "DELETE" && id) {
    const res = await env.DB.prepare(
      `UPDATE tokens SET revoked_at = ?1 WHERE id = ?2 AND account_id = ?3 AND kind = 'pat' AND revoked_at IS NULL`,
    )
      .bind(now, id, account.id)
      .run();
    if (!res.meta.changes) throw new RegistryError(404, "NOT_FOUND", "no such token");
    return new Response(null, { status: 204 });
  }
  throw new RegistryError(405, "METHOD", "method not allowed");
}
