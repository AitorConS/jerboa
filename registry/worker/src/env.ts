export interface Env {
  BLOBS: R2Bucket;
  DB: D1Database;
  /** Public base URL pulls read from (R2 behind the CDN). */
  CDN_BASE: string;
  /** GitHub REST API base, overridable for tests. */
  GITHUB_API: string;
  /** OAuth App client id clients use for the device flow. */
  GITHUB_CLIENT_ID: string;
  /** Storage each namespace may hold, in bytes of packs it uploaded first. */
  NAMESPACE_QUOTA_BYTES: string;
  /** Secret that signs push tokens. */
  JWT_SIGNING_KEY: string;
}
