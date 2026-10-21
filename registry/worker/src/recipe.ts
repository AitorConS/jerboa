import { sha256Digest } from "./crypto";
import { invalid, tooLarge } from "./errors";
import { isDigest } from "./names";

export const MEDIA_TYPE = "application/vnd.jerboa.image.recipe.v1+json";
export const MAX_RECIPE = 16 << 20;
export const MAX_BLOBS = 100_000;

export interface RecipeBlob {
  digest: string;
  size: number;
}

/** What the hub shows about a recipe (recipes.summary). */
export interface RecipeSummary {
  platform: string;
  layout: string;
  memory: string;
  cpus: number;
  env: string[];
  ports: string[];
  packages: { name: string; version: string }[];
  toolset: string;
  diskSize: number;
  blobCount: number;
  blobBytes: number;
}

const MAX_LISTED = 200;
const str = (v: unknown) => (typeof v === "string" ? v.slice(0, 256) : "");
const num = (v: unknown) => (Number.isSafeInteger(v) && (v as number) >= 0 ? (v as number) : 0);
const strings = (v: unknown) => (Array.isArray(v) ? v.filter((s) => typeof s === "string").slice(0, MAX_LISTED).map(str) : []);

/**
 * Summarizes a parsed recipe. checkRecipe does not validate the fields shown
 * here, so every one is read defensively.
 */
export function summarize(r: Record<string, unknown>): RecipeSummary {
  const config = (r.config ?? {}) as Record<string, unknown>;
  const toolset = (r.toolset ?? {}) as Record<string, unknown>;
  const blobs = Array.isArray(r.blobs) ? (r.blobs as { size?: unknown }[]) : [];
  const packages = Array.isArray(r.packages) ? (r.packages as Record<string, unknown>[]) : [];
  return {
    platform: str(r.platform),
    layout: str(r.layout),
    memory: str(config.memory),
    cpus: num(config.cpus),
    env: strings(config.env),
    ports: strings(config.ports),
    packages: packages.slice(0, MAX_LISTED).map((p) => ({ name: str(p?.name), version: str(p?.version) })),
    toolset: str(toolset.version),
    diskSize: num(r.diskSize),
    blobCount: blobs.length,
    blobBytes: blobs.reduce((n, b) => n + num(b?.size), 0),
  };
}

export function parseRecipe(bytes: Uint8Array): Record<string, unknown> {
  try {
    return JSON.parse(new TextDecoder().decode(bytes));
  } catch {
    throw invalid("RECIPE_INVALID", "recipe is not JSON");
  }
}

/**
 * Checks what the registry relies on: the bytes match the digest in the URL,
 * the schema is known, and the blob list is well formed. Clients run the full
 * validation (recipe.Parse) and verify the assembled disk themselves.
 */
export async function checkRecipe(bytes: Uint8Array, digest: string): Promise<{ blobs: RecipeBlob[]; summary: RecipeSummary }> {
  if (bytes.length > MAX_RECIPE) throw tooLarge(`recipe is over ${MAX_RECIPE} bytes`);
  if ((await sha256Digest(bytes)) !== digest) throw invalid("RECIPE_INVALID", "recipe bytes do not match the digest");
  const r = parseRecipe(bytes);
  if (r.schemaVersion !== 1 || r.mediaType !== MEDIA_TYPE) throw invalid("RECIPE_INVALID", "unsupported recipe schema");
  if (!isDigest(r.diskDigest) || typeof r.manifest !== "string") throw invalid("RECIPE_INVALID", "recipe is incomplete");
  const toolset = r.toolset as Record<string, unknown> | undefined;
  if (!toolset || typeof toolset.version !== "string" || !isDigest(toolset.boot) || !isDigest(toolset.kernel)) {
    throw invalid("RECIPE_INVALID", "recipe toolset is incomplete");
  }
  if (!Array.isArray(r.blobs) || r.blobs.length === 0) throw invalid("RECIPE_INVALID", "recipe lists no blobs");
  if (r.blobs.length > MAX_BLOBS) throw tooLarge(`recipe lists over ${MAX_BLOBS} blobs`);
  let prev = "";
  const blobs: RecipeBlob[] = [];
  for (const b of r.blobs as RecipeBlob[]) {
    if (!isDigest(b?.digest) || !Number.isSafeInteger(b?.size) || b.size < 0) throw invalid("RECIPE_INVALID", "invalid blob");
    if (b.digest <= prev) throw invalid("RECIPE_INVALID", "blobs must be sorted and unique");
    prev = b.digest;
    blobs.push({ digest: b.digest, size: b.size });
  }
  return { blobs, summary: summarize(r) };
}
