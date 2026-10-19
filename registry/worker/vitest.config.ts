import { cloudflareTest, readD1Migrations } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

// A stand-in for api.github.com: tokens name the user they belong to.
const GITHUB_USERS: Record<string, { id: number; login: string; orgs: string[] }> = {
  "gh-alice": { id: 1, login: "Alice", orgs: ["Acme"] },
  "gh-bob": { id: 2, login: "bob", orgs: [] },
};

function fakeGitHub(req: Request): Response {
  const url = new URL(req.url);
  if (url.host !== "github.test") return new Response("unexpected outbound request to " + url.host, { status: 599 });
  const user = GITHUB_USERS[(req.headers.get("Authorization") ?? "").replace("Bearer ", "")];
  if (!user) return new Response("bad credentials", { status: 401 });
  if (url.pathname === "/user") return Response.json({ id: user.id, login: user.login });
  if (url.pathname === "/user/orgs") return Response.json(user.orgs.map((login) => ({ login })));
  return new Response("not found", { status: 404 });
}

export default defineConfig(async () => {
  const migrations = await readD1Migrations("./migrations");
  return {
    plugins: [
      cloudflareTest({
        wrangler: { configPath: "./wrangler.toml" },
        miniflare: {
          bindings: {
            GITHUB_API: "https://github.test",
            JWT_SIGNING_KEY: "test-signing-key-that-is-long-enough-0123456789",
            TEST_MIGRATIONS: migrations,
          },
          outboundService: fakeGitHub,
        },
      }),
    ],
    test: { setupFiles: ["./test/setup.ts"] },
  };
});
