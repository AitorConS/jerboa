import { SELF } from "cloudflare:test";
import { expect } from "vitest";
import fixture from "./fixtures/go.json";

export { fixture };

export const BASE = "https://registry.test";
export const b64 = (s: string) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
export const upload = b64(fixture.upload);
export const recipeBytes = b64(fixture.recipe);
export const hex = (d: string) => d.slice("sha256:".length);

export async function call(method: string, path: string, init: { token?: string; json?: unknown; body?: BodyInit } = {}) {
  const headers: Record<string, string> = {};
  if (init.token) headers.Authorization = `Bearer ${init.token}`;
  let body = init.body;
  if (init.json !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(init.json);
  }
  return SELF.fetch(BASE + path, { method, headers, body });
}

export async function login(gh = "gh-alice"): Promise<string> {
  const res = await call("POST", "/auth/github", { json: { access_token: gh } });
  expect(res.status).toBe(200);
  return ((await res.json()) as { token: string }).token;
}

export async function pushToken(refresh: string, repo = "alice/web"): Promise<string> {
  const res = await call("POST", "/auth/token", { token: refresh, json: { scopes: [`repository:${repo}:push`] } });
  expect(res.status).toBe(200);
  return ((await res.json()) as { token: string }).token;
}

export async function errorCode(res: Response): Promise<string> {
  return ((await res.json()) as { error: { code: string } }).error.code;
}

/** Frames an upload body for an index and pack bytes. */
export function frame(index: unknown, data: Uint8Array): Uint8Array {
  const idx = new TextEncoder().encode(JSON.stringify(index));
  const out = new Uint8Array(4 + idx.length + data.length);
  new DataView(out.buffer).setUint32(0, idx.length);
  out.set(idx, 4);
  out.set(data, 4 + idx.length);
  return out;
}

export async function pushFixture(token: string, repo = "alice/web", tag = "latest") {
  expect((await call("PUT", `/v2/${repo}/packs/${hex(fixture.pack)}`, { token, body: upload })).status).toBeLessThan(300);
  expect((await call("PUT", `/v2/${repo}/recipes/${hex(fixture.recipeDigest)}`, { token, body: recipeBytes })).status).toBeLessThan(300);
  expect((await call("PUT", `/v2/${repo}/tags/${tag}`, { token, json: { recipe: fixture.recipeDigest } })).status).toBe(200);
}
