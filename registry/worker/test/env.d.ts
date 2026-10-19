import type { D1Migration } from "cloudflare:test";
import type { Env as WorkerEnv } from "../src/env";

// The bindings tests see: the Worker's, plus the migrations to apply.
declare global {
  namespace Cloudflare {
    interface Env extends WorkerEnv {
      TEST_MIGRATIONS: D1Migration[];
    }
  }
}
