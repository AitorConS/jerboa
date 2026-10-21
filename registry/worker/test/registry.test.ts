import { env, SELF } from "cloudflare:test";
import { decompress } from "fzstd";
import { describe, expect, it } from "vitest";
import { sha256Digest } from "../src/crypto";
import { collectGarbage } from "../src/gc";
import { b64, BASE, call, errorCode, fixture, frame, hex, login, pushFixture, pushToken, recipeBytes, upload } from "./helpers";

describe("discovery and auth", () => {
  it("serves discovery", async () => {
    const res = await call("GET", "/v2/");
    expect(await res.json()).toMatchObject({ api: 1, github_client_id: "local-development" });
  });

  it("grants push only to the user's own namespace and organizations", async () => {
    const refresh = await login();
    expect(refresh).toMatch(/^jrt_/);
    await pushToken(refresh, "alice/web");
    await pushToken(refresh, "acme/web"); // org membership, lowercased
    const res = await call("POST", "/auth/token", { token: refresh, json: { scopes: ["repository:bob/web:push"] } });
    expect(res.status).toBe(403);
    expect((await call("POST", "/auth/github", { json: { access_token: "unknown" } })).status).toBe(401);
    expect((await call("POST", "/auth/token", { token: "jrt_forged", json: { scopes: ["repository:alice/web:push"] } })).status).toBe(401);
  });

  it("issues, uses and revokes personal access tokens", async () => {
    const refresh = await login();
    const created = await call("POST", "/auth/pats", { token: refresh, json: { name: "ci" } });
    expect(created.status).toBe(201);
    const pat = (await created.json()) as { id: string; token: string };
    expect(pat.token).toMatch(/^jpat_/);
    await pushToken(pat.token);
    const listed = (await (await call("GET", "/auth/pats", { token: refresh })).json()) as { pats: { id: string }[] };
    expect(listed.pats.map((p) => p.id)).toEqual([pat.id]);
    expect((await call("DELETE", `/auth/pats/${pat.id}`, { token: refresh })).status).toBe(204);
    const res = await call("POST", "/auth/token", { token: pat.token, json: { scopes: ["repository:alice/web:push"] } });
    expect(res.status).toBe(401);
  });
});

describe("push and pull", () => {
  it("accepts a Go-built pack, recipe and tag, and serves them for pulls", async () => {
    const token = await pushToken(await login());
    const digests = (fixture.entries as { digest: string }[]).map((e) => e.digest);
    const missing = await call("POST", "/v2/alice/web/blobs/missing", { token, json: { digests } });
    expect(((await missing.json()) as { missing: string[] }).missing.sort()).toEqual([...digests].sort());

    await pushFixture(token);

    const after = await call("POST", "/v2/alice/web/blobs/missing", { token, json: { digests } });
    expect(((await after.json()) as { missing: string[] }).missing).toEqual([]);

    // The static layout a pull reads.
    const tag = await env.BLOBS.get("tags/alice/web/latest");
    expect(await tag!.json()).toEqual({ recipe: fixture.recipeDigest });
    expect(tag!.httpMetadata?.cacheControl).toBe("public, max-age=60");
    const stored = await env.BLOBS.get(`recipes/${hex(fixture.recipeDigest)}.json`);
    expect(new Uint8Array(await stored!.arrayBuffer())).toEqual(recipeBytes);
    const locations = (await (await env.BLOBS.get(`locations/${hex(fixture.recipeDigest)}.json`))!.json()) as {
      blobs: Record<string, { pack: string; offset: number; length: number }>;
    };
    for (const e of fixture.entries as { digest: string; offset: number; length: number; size: number }[]) {
      expect(locations.blobs[e.digest]).toEqual({ pack: fixture.pack, offset: e.offset, length: e.length });
      // A range request for one frame yields exactly that blob.
      const res = await call("GET", `/cdn/packs/${hex(fixture.pack)}`, {});
      expect(res.status).toBe(200);
      const ranged = await SELF.fetch(`${BASE}/cdn/packs/${hex(fixture.pack)}`, {
        headers: { Range: `bytes=${e.offset}-${e.offset + e.length - 1}` },
      });
      expect(ranged.status).toBe(206);
      const blob = e.size === 0 ? new Uint8Array() : decompress(new Uint8Array(await ranged.arrayBuffer()));
      expect(await sha256Digest(blob)).toBe(e.digest);
    }
    const tags = (await (await call("GET", "/v2/alice/web/tags")).json()) as { tags: { tag: string; recipe: string }[] };
    expect(tags.tags).toMatchObject([{ tag: "latest", recipe: fixture.recipeDigest }]);

    // Pushing the same pack again changes nothing.
    const again = await call("PUT", `/v2/alice/web/packs/${hex(fixture.pack)}`, { token, body: upload });
    expect(again.status).toBe(200);
  });

  it("requires a push token for the repository", async () => {
    const other = await pushToken(await login(), "alice/other");
    const res = await call("PUT", `/v2/alice/web/packs/${hex(fixture.pack)}`, { token: other, body: upload });
    expect(res.status).toBe(403);
    expect((await call("PUT", `/v2/alice/web/packs/${hex(fixture.pack)}`, { body: upload })).status).toBe(401);
  });
});

describe("verification", () => {
  it("rejects a blob registered under someone else's digest", async () => {
    const token = await pushToken(await login());
    const n = new DataView(upload.buffer).getUint32(0);
    const index = JSON.parse(new TextDecoder().decode(upload.subarray(4, 4 + n)));
    const data = upload.subarray(4 + n);
    index.blobs[0].digest = await sha256Digest(new TextEncoder().encode("victim"));
    const res = await call("PUT", `/v2/alice/web/packs/${hex(fixture.pack)}`, { token, body: frame(index, data) });
    expect(res.status).toBe(400);
    expect(await errorCode(res)).toBe("BLOB_INVALID");
  });

  it("stops a decompression bomb at its declared size", async () => {
    const token = await pushToken(await login());
    const data = b64(fixture.bombFrame);
    const digest = await sha256Digest(data);
    const index = { digest, size: data.length, blobs: [{ digest: fixture.bombDigest, size: 5, offset: 0, length: data.length }] };
    const res = await call("PUT", `/v2/alice/web/packs/${hex(digest)}`, { token, body: frame(index, data) });
    expect(res.status).toBe(400);
    expect(await errorCode(res)).toBe("BLOB_INVALID");
  });

  it("rejects a pack whose bytes do not match the URL or its index", async () => {
    const token = await pushToken(await login());
    const wrongURL = await call("PUT", `/v2/alice/web/packs/${"0".repeat(64)}`, { token, body: upload });
    expect(await errorCode(wrongURL)).toBe("PACK_INVALID");
    const tampered = upload.slice();
    tampered[tampered.length - 1]! ^= 1;
    const res = await call("PUT", `/v2/alice/web/packs/${hex(fixture.pack)}`, { token, body: tampered });
    expect(await errorCode(res)).toBe("PACK_INVALID");
  });

  it("rejects recipes with unknown blobs or wrong bytes, and tags to unknown recipes", async () => {
    const token = await pushToken(await login());
    const unknown = await call("PUT", `/v2/alice/web/recipes/${hex(fixture.recipeDigest)}`, { token, body: recipeBytes });
    expect(unknown.status).toBe(409);
    expect(await errorCode(unknown)).toBe("BLOB_UNKNOWN");
    const wrong = await call("PUT", `/v2/alice/web/recipes/${"1".repeat(64)}`, { token, body: recipeBytes });
    expect(await errorCode(wrong)).toBe("RECIPE_INVALID");
    const tag = await call("PUT", "/v2/alice/web/tags/v1", { token, json: { recipe: fixture.recipeDigest } });
    expect(await errorCode(tag)).toBe("RECIPE_UNKNOWN");
  });

  it("enforces the namespace quota", async () => {
    await env.DB.prepare(`INSERT INTO packs (digest, size, namespace, created_at) VALUES (?1, ?2, 'alice', 0)`)
      .bind("sha256:" + "f".repeat(64), Number(env.NAMESPACE_QUOTA_BYTES))
      .run();
    const token = await pushToken(await login());
    const res = await call("PUT", `/v2/alice/web/packs/${hex(fixture.pack)}`, { token, body: upload });
    expect(res.status).toBe(403);
    expect(await errorCode(res)).toBe("QUOTA");
  });
});

describe("garbage collection", () => {
  it("keeps tagged content and collects it a week after its tag is deleted", async () => {
    const token = await pushToken(await login());
    await pushFixture(token);
    const day = 24 * 3600;
    const t0 = Math.floor(Date.now() / 1000);
    expect(await collectGarbage(env, t0 + 30 * day)).toMatchObject({ recipes: 0, packs: 0 });

    expect((await call("DELETE", "/v2/alice/web/tags/latest", { token })).status).toBe(204);
    expect(await env.BLOBS.get("tags/alice/web/latest")).toBeNull();
    expect(await collectGarbage(env, t0 + day)).toMatchObject({ recipes: 0 }); // within the grace period
    expect(await collectGarbage(env, t0 + 8 * day)).toMatchObject({ recipes: 1, packs: 0 });
    expect(await env.BLOBS.get(`recipes/${hex(fixture.recipeDigest)}.json`)).toBeNull();
    expect(await collectGarbage(env, t0 + 9 * day)).toMatchObject({ packs: 0 }); // pack clock just started
    expect(await collectGarbage(env, t0 + 17 * day)).toMatchObject({ packs: 1 });
    expect(await env.BLOBS.get(`packs/${hex(fixture.pack)}`)).toBeNull();
    const left = await env.DB.prepare(`SELECT COUNT(*) AS n FROM blobs`).first<{ n: number }>();
    expect(left?.n).toBe(0);
  });
});
