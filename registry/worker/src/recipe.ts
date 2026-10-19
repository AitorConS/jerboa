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

/**
 * Checks what the registry relies on: the bytes match the digest in the URL,
 * the schema is known, and the blob list is well formed. Clients run the full
 * validation (recipe.Parse) and verify the assembled disk themselves.
 */
export async function checkRecipe(bytes: Uint8Array, digest: string): Promise<RecipeBlob[]> {
  if (bytes.length > MAX_RECIPE) throw tooLarge(`recipe is over ${MAX_RECIPE} bytes`);
  if ((await sha256Digest(bytes)) !== digest) throw invalid("RECIPE_INVALID", "recipe bytes do not match the digest");
  let r: Record<string, unknown>;
  try {
    r = JSON.parse(new TextDecoder().decode(bytes));
  } catch {
    throw invalid("RECIPE_INVALID", "recipe is not JSON");
  }
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
  return blobs;
}
