import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { call, errorCode, fixture, login, pushFixture, pushToken } from "./helpers";

interface Card {
  namespace: string;
  name: string;
  description: string;
  pulls: number;
  updated: number;
}

async function get<T>(path: string): Promise<T> {
  const res = await call("GET", path);
  expect(res.status, path).toBe(200);
  expect(res.headers.get("Cache-Control")).toBe("public, max-age=60");
  return (await res.json()) as T;
}

const names = (repos: Card[]) => repos.map((r) => `${r.namespace}/${r.name}`);

/** Pushes the fixture to each repository, all as alice (who belongs to acme). */
async function pushAll(...repos: string[]) {
  const refresh = await login();
  for (const repo of repos) await pushFixture(await pushToken(refresh, repo), repo);
}

describe("catalog", () => {
  it("describes a repository and summarizes its tags", async () => {
    const before = Math.floor(Date.now() / 1000);
    await pushAll("alice/web");
    const repo = await get<Card & { readme: string; tags: number; created: number }>("/v2/alice/web");
    expect(repo).toMatchObject({ namespace: "alice", name: "web", description: "", readme: "", pulls: 0, tags: 1 });
    expect(repo.updated).toBeGreaterThanOrEqual(before);

    const tag = await get<{ recipe: string; summary: unknown }>("/v2/alice/web/tags/latest");
    expect(tag).toMatchObject({
      tag: "latest",
      recipe: fixture.recipeDigest,
      summary: {
        platform: "linux/amd64",
        layout: "compact",
        memory: "256M",
        cpus: 1,
        env: [],
        ports: [],
        packages: [],
        toolset: "v0.2.3",
        diskSize: 4,
        blobCount: 5,
        blobBytes: 104040,
      },
    });

    expect((await call("GET", "/v2/alice/nope")).status).toBe(404);
    expect((await call("GET", "/v2/alice/web/tags/nope")).status).toBe(404);
    expect(await errorCode(await call("GET", "/v2/Alice/web"))).toBe("NAME_INVALID");
  });

  it("summarizes recipes pushed before summaries existed on first view", async () => {
    await pushAll("alice/web");
    await env.DB.prepare(`UPDATE recipes SET summary = NULL`).run();
    const tag = await get<{ summary: { memory: string } }>("/v2/alice/web/tags/latest");
    expect(tag.summary.memory).toBe("256M");
    const stored = await env.DB.prepare(`SELECT summary FROM recipes`).first<{ summary: string | null }>();
    expect(JSON.parse(stored!.summary!)).toEqual(tag.summary);
  });

  it("lists a namespace's repositories, newest first, without emptied ones", async () => {
    await pushAll("alice/web", "alice/api", "acme/web");
    await env.DB.prepare(`UPDATE repos SET updated_at = 1 WHERE name = 'web'`).run();
    expect(names((await get<{ repos: Card[] }>("/v2/alice")).repos)).toEqual(["alice/api", "alice/web"]);

    const token = await pushToken(await login(), "alice/api");
    expect((await call("DELETE", "/v2/alice/api/tags/latest", { token })).status).toBe(204);
    expect(names((await get<{ repos: Card[] }>("/v2/alice")).repos)).toEqual(["alice/web"]);
    // The repository itself remains, with no tags, and its update time moved.
    const api = await get<Card & { tags: number }>("/v2/alice/api");
    expect(api.tags).toBe(0);
    expect(api.updated).toBeGreaterThan(1);

    expect((await call("GET", "/v2/nobody")).status).toBe(404);
  });

  it("searches names and descriptions", async () => {
    await pushAll("alice/web", "alice/api", "acme/tools");
    await env.DB.batch([
      env.DB.prepare(`UPDATE repos SET description = 'A 100% static Web server', pulls = 5 WHERE name = 'api'`),
      env.DB.prepare(`UPDATE repos SET pulls = 9 WHERE name = 'tools'`),
      env.DB.prepare(`UPDATE repos SET updated_at = 1 WHERE name = 'tools'`),
    ]);
    const search = async (query: string) => (await get<{ repos: Card[]; next: boolean }>(`/v2/_search?${query}`)).repos;

    expect(names(await search(""))).toEqual(["acme/tools", "alice/api", "alice/web"]);
    expect(names(await search("q=WEB"))).toEqual(["alice/api", "alice/web"]);
    expect(names(await search("q=acme/"))).toEqual(["acme/tools"]);
    expect(names(await search("q=100%25"))).toEqual(["alice/api"]);
    expect(names(await search("q=%25"))).toEqual(["alice/api"]); // a literal %, not a wildcard
    expect(names(await search("q=_"))).toEqual([]);
    expect(names(await search("sort=updated")).at(-1)).toBe("acme/tools");

    expect(await errorCode(await call("GET", "/v2/_search?page=0"))).toBe("BAD_REQUEST");
    expect(await errorCode(await call("GET", `/v2/_search?q=${"x".repeat(101)}`))).toBe("BAD_REQUEST");
  });

  it("pages search results", async () => {
    await pushAll("alice/web");
    const now = Math.floor(Date.now() / 1000);
    const stmts = [];
    for (let i = 0; i < 24; i++) {
      stmts.push(
        env.DB.prepare(`INSERT INTO repos (namespace, name, created_at, updated_at) VALUES ('bulk', ?1, ?2, ?2)`).bind(`r${i}`, now),
        env.DB.prepare(
          `INSERT INTO tags (repo_id, tag, recipe, updated_at) SELECT id, 'latest', ?1, ?2 FROM repos WHERE namespace = 'bulk' AND name = ?3`,
        ).bind(fixture.recipeDigest, now, `r${i}`),
      );
    }
    await env.DB.batch(stmts);
    const first = await get<{ repos: Card[]; next: boolean }>("/v2/_search");
    const second = await get<{ repos: Card[]; next: boolean }>("/v2/_search?page=2");
    expect([first.repos.length, first.next, second.repos.length, second.next]).toEqual([20, true, 5, false]);
    expect(new Set([...names(first.repos), ...names(second.repos)]).size).toBe(25);
  });
});
