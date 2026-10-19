import { invalid } from "./errors";

const NAMESPACE = /^[a-z0-9](?:[a-z0-9-]{0,38})$/;
const REPOSITORY = /^[a-z0-9]+(?:[._-][a-z0-9]+)*$/;
const TAG = /^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$/;
const HEX = /^[0-9a-f]{64}$/;
const DIGEST = /^sha256:[0-9a-f]{64}$/;

export function checkRepo(namespace: string, repo: string): void {
  if (!NAMESPACE.test(namespace)) throw invalid("NAME_INVALID", `invalid namespace ${JSON.stringify(namespace)}`);
  if (repo.length > 128 || !REPOSITORY.test(repo)) throw invalid("NAME_INVALID", `invalid repository ${JSON.stringify(repo)}`);
}

export function checkTag(tag: string): void {
  if (!TAG.test(tag)) throw invalid("NAME_INVALID", `invalid tag ${JSON.stringify(tag)}`);
}

export function isDigest(d: unknown): d is string {
  return typeof d === "string" && DIGEST.test(d);
}

/** Returns the digest for the hex in a URL, or throws. */
export function digestFromHex(hex: string, code: string): string {
  if (!HEX.test(hex)) throw invalid(code, "invalid digest in URL");
  return "sha256:" + hex;
}

export const hexOf = (digest: string) => digest.slice("sha256:".length);
