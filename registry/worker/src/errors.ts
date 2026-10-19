/** An error the protocol reports to clients (docs/registry-protocol.md). */
export class RegistryError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly headers: Record<string, string> = {},
  ) {
    super(message);
  }
}

export function errorResponse(e: RegistryError): Response {
  return Response.json({ error: { code: e.code, message: e.message } }, { status: e.status, headers: e.headers });
}

export const unauthorized = (m = "missing or invalid token") =>
  new RegistryError(401, "UNAUTHORIZED", m, { "WWW-Authenticate": 'Bearer realm="jerboa"' });
export const denied = (m: string) => new RegistryError(403, "DENIED", m);
export const invalid = (code: string, m: string) => new RegistryError(400, code, m);
export const tooLarge = (m: string) => new RegistryError(413, "TOO_LARGE", m);
