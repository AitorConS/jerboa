import { issuePushToken, loginWithGitHub, nowSeconds, pats } from "./auth";
import type { Env } from "./env";
import { catalog } from "./catalog";
import { errorResponse, RegistryError } from "./errors";
import { collectGarbage } from "./gc";
import { deleteTag, listTags, missingBlobs, putPack, putRecipe, putTag } from "./push";

export type { Env };

const API_VERSION = 1;

async function route(req: Request, env: Env): Promise<Response> {
  const url = new URL(req.url);
  const parts = url.pathname.split("/").filter(Boolean).map(decodeURIComponent);
  const m = req.method;

  if (parts[0] === "v2" && parts.length === 1 && m === "GET") {
    return Response.json(
      { api: API_VERSION, cdn: env.CDN_BASE, github_client_id: env.GITHUB_CLIENT_ID },
      { headers: { "Cache-Control": "public, max-age=300" } },
    );
  }
  if (parts[0] === "auth") {
    if (parts[1] === "github" && parts.length === 2 && m === "POST") return loginWithGitHub(req, env);
    if (parts[1] === "token" && parts.length === 2 && m === "POST") return issuePushToken(req, env);
    if (parts[1] === "pats" && parts.length <= 3) return pats(req, env, parts[2]);
  }
  if (parts[0] === "v2" && parts.length >= 4) {
    const [, ns, repo, kind, arg] = parts as [string, string, string, string, string | undefined];
    if (kind === "blobs" && arg === "missing" && parts.length === 5 && m === "POST") return missingBlobs(req, env, ns, repo);
    if (kind === "packs" && arg && parts.length === 5 && m === "PUT") return putPack(req, env, ns, repo, arg);
    if (kind === "recipes" && arg && parts.length === 5 && m === "PUT") return putRecipe(req, env, ns, repo, arg);
    if (kind === "tags" && parts.length === 4 && m === "GET") return listTags(env, ns, repo);
    if (kind === "tags" && arg && parts.length === 5 && m === "PUT") return putTag(req, env, ns, repo, arg);
    if (kind === "tags" && arg && parts.length === 5 && m === "DELETE") return deleteTag(req, env, ns, repo, arg);
  }
  if (parts[0] === "v2" && parts.length >= 2 && m === "GET") return catalog(url, env, parts.slice(1));
  // Local development only: serve the R2 layout the way the CDN would.
  if (parts[0] === "cdn" && env.CDN_BASE.endsWith("/cdn") && (m === "GET" || m === "HEAD")) {
    return serveCDN(req, env, parts.slice(1).join("/"));
  }
  throw new RegistryError(404, "NOT_FOUND", "no such endpoint");
}

async function serveCDN(req: Request, env: Env, key: string): Promise<Response> {
  const range = req.headers.get("Range");
  const m = range ? /^bytes=(\d+)-(\d+)$/.exec(range) : null;
  const obj = await env.BLOBS.get(key, m ? { range: { offset: Number(m[1]), length: Number(m[2]) - Number(m[1]) + 1 } } : {});
  if (!obj) return new Response("not found", { status: 404 });
  const headers = new Headers();
  obj.writeHttpMetadata(headers);
  if (m) headers.set("Content-Range", `bytes ${m[1]}-${m[2]}/${obj.size}`);
  return new Response(req.method === "HEAD" ? null : obj.body, { status: m ? 206 : 200, headers });
}

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    try {
      return await route(req, env);
    } catch (e) {
      if (e instanceof RegistryError) return errorResponse(e);
      console.error("internal error", e);
      return errorResponse(new RegistryError(500, "INTERNAL", "internal error"));
    }
  },
  async scheduled(_event: ScheduledController, env: Env, ctx: ExecutionContext): Promise<void> {
    ctx.waitUntil(
      collectGarbage(env, nowSeconds()).then((r) => console.log("gc", JSON.stringify(r))),
    );
  },
} satisfies ExportedHandler<Env>;
