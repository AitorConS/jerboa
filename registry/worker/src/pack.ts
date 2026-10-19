import { Decompress } from "fzstd";
import { sha256Digest } from "./crypto";
import { invalid, tooLarge } from "./errors";
import { isDigest } from "./names";

export const MAX_PACK = 64 << 20;
const MAX_INDEX = 16 << 20;

export interface Entry {
  digest: string;
  size: number;
  offset: number;
  length: number;
}

export interface Pack {
  digest: string;
  size: number;
  blobs: Entry[];
  data: Uint8Array;
}

/**
 * Parses an upload body (4-byte big-endian index length, JSON index, pack
 * bytes) and verifies the pack completely before anything is trusted: its
 * digest and size, frames that tile it in order, and every blob decompressed
 * to exactly its declared size and checked against its digest. Mirrors
 * pack.DecodeUpload in the Go client.
 */
export async function decodeUpload(body: Uint8Array): Promise<Pack> {
  if (body.length < 4) throw invalid("PACK_INVALID", "upload too short");
  const n = new DataView(body.buffer, body.byteOffset, body.byteLength).getUint32(0);
  if (n > MAX_INDEX || n > body.length - 4) throw invalid("PACK_INVALID", "invalid index length");
  let index: { digest?: unknown; size?: unknown; blobs?: unknown };
  try {
    index = JSON.parse(new TextDecoder().decode(body.subarray(4, 4 + n)));
  } catch {
    throw invalid("PACK_INVALID", "index is not JSON");
  }
  const data = body.subarray(4 + n);
  if (data.length === 0 || data.length > MAX_PACK) throw tooLarge(`pack size ${data.length} outside 1..${MAX_PACK}`);
  if (!isDigest(index.digest) || index.size !== data.length || !Array.isArray(index.blobs)) {
    throw invalid("PACK_INVALID", "index digest, size or blobs are wrong");
  }
  if ((await sha256Digest(data)) !== index.digest) throw invalid("PACK_INVALID", "digest does not match its bytes");
  const seen = new Set<string>();
  let next = 0;
  const blobs: Entry[] = [];
  for (const raw of index.blobs as Entry[]) {
    const e = { digest: raw?.digest, size: raw?.size, offset: raw?.offset, length: raw?.length };
    if (!isDigest(e.digest) || !Number.isSafeInteger(e.size) || e.size < 0 || e.size > 2 ** 31) {
      throw invalid("PACK_INVALID", "invalid blob entry");
    }
    if (e.offset !== next || !Number.isSafeInteger(e.length) || e.length <= 0 || e.offset + e.length > data.length) {
      throw invalid("PACK_INVALID", `blob ${e.digest} frame does not follow the previous one`);
    }
    if (seen.has(e.digest)) throw invalid("PACK_INVALID", `blob ${e.digest} listed twice`);
    seen.add(e.digest);
    const out = inflate(data.subarray(e.offset, e.offset + e.length), e.size, e.digest);
    if ((await sha256Digest(out)) !== e.digest) {
      throw invalid("BLOB_INVALID", `blob ${e.digest} does not match its digest`);
    }
    blobs.push(e as Entry);
    next = e.offset + e.length;
  }
  if (next !== data.length) throw invalid("PACK_INVALID", "bytes after the last blob");
  return { digest: index.digest, size: data.length, blobs, data };
}

/** Decompresses one frame into exactly size bytes, failing as soon as it grows past them. */
function inflate(frame: Uint8Array, size: number, digest: string): Uint8Array {
  const out = new Uint8Array(size);
  let at = 0;
  const d = new Decompress((chunk) => {
    if (at + chunk.length > size) throw invalid("BLOB_INVALID", `blob ${digest} is larger than its declared ${size} bytes`);
    out.set(chunk, at);
    at += chunk.length;
  });
  try {
    d.push(frame, true);
  } catch (e) {
    if (e instanceof Error && "status" in e) throw e;
    throw invalid("BLOB_INVALID", `blob ${digest} is not a valid zstd frame`);
  }
  if (at !== size) throw invalid("BLOB_INVALID", `blob ${digest} is ${at} bytes, index says ${size}`);
  return out;
}
