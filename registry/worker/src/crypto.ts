const enc = new TextEncoder();

export async function sha256Hex(data: ArrayBuffer | ArrayBufferView): Promise<string> {
  const buf = await crypto.subtle.digest("SHA-256", data);
  return [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export const sha256Digest = async (data: ArrayBuffer | ArrayBufferView) => "sha256:" + (await sha256Hex(data));

export function base64url(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function fromBase64url(s: string): Uint8Array {
  const b = atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4));
  return Uint8Array.from(b, (c) => c.charCodeAt(0));
}

/** A random bearer token with a recognizable prefix. */
export function randomToken(prefix: string): string {
  return prefix + base64url(crypto.getRandomValues(new Uint8Array(32)));
}

async function hmacKey(secret: string): Promise<CryptoKey> {
  if (!secret || secret.length < 32) throw new Error("JWT_SIGNING_KEY must be at least 32 characters");
  return crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign", "verify"]);
}

export interface PushClaims {
  sub: number;
  login: string;
  scopes: string[];
  iat: number;
  exp: number;
  iss: string;
}

export async function signJWT(claims: PushClaims, secret: string): Promise<string> {
  const head = base64url(enc.encode(JSON.stringify({ alg: "HS256", typ: "JWT" })));
  const body = base64url(enc.encode(JSON.stringify(claims)));
  const sig = await crypto.subtle.sign("HMAC", await hmacKey(secret), enc.encode(`${head}.${body}`));
  return `${head}.${body}.${base64url(new Uint8Array(sig))}`;
}

/** Verifies an HS256 JWT and its expiry; returns its claims or null. */
export async function verifyJWT(token: string, secret: string, now: number): Promise<PushClaims | null> {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  const [head, body, sig] = parts as [string, string, string];
  let header: { alg?: string };
  try {
    header = JSON.parse(new TextDecoder().decode(fromBase64url(head)));
  } catch {
    return null;
  }
  if (header.alg !== "HS256") return null;
  const ok = await crypto.subtle.verify("HMAC", await hmacKey(secret), fromBase64url(sig), enc.encode(`${head}.${body}`));
  if (!ok) return null;
  const claims = JSON.parse(new TextDecoder().decode(fromBase64url(body))) as PushClaims;
  if (typeof claims.exp !== "number" || claims.exp <= now || !Array.isArray(claims.scopes)) return null;
  return claims;
}
