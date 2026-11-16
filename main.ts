import { createApp } from "./app.ts";

// One register or done request may run this long before it is dropped and retried.
const COORDINATOR_TIMEOUT_MS = 30_000;
// Backoff starts here and never grows past the cap below.
const RETRY_BASE_MS = 250;
const RETRY_CAP_MS = 2_000;
// The doubling stops after this many failures.
const RETRY_MAX_STEP = 8;

function envOrigin(): string {
  return (process.env.MAKEFUN_ENV_URL ?? "").replace(/\/+$/, "");
}

function backoffMs(failures: number): number {
  const step = Math.min(RETRY_MAX_STEP, failures);
  return Math.min(RETRY_CAP_MS, RETRY_BASE_MS * 2 ** step);
}

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function requestCoordinator(origin: string, path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(origin + path, { ...init, signal: AbortSignal.timeout(COORDINATOR_TIMEOUT_MS) });
}

async function registerWithEnv(origin: string, port: number): Promise<void> {
  let failures = 0;
  for (;;) {
    let response: Response | undefined;
    try {
      response = await requestCoordinator(origin, "/register", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ port }),
      });
    } catch {
      // The coordinator may still be starting; fall through and retry.
    }
    if (response) {
      if (response.status === 200 || response.status === 409) return;
      if (response.status === 400 || response.status === 405) {
        throw new Error(`Registration rejected: HTTP ${response.status}: ${await response.text()}`);
      }
    }
    await wait(backoffMs(failures++));
  }
}

async function waitUntilDone(origin: string): Promise<void> {
  let failures = 0;
  for (;;) {
    try {
      const response = await requestCoordinator(origin, "/done");
      if (response.ok) {
        const payload: unknown = await response.json();
        if (typeof payload === "object" && payload !== null && "done" in payload && payload.done === true) return;
      }
    } catch {
      // A failed poll is no evidence the run finished; keep polling.
    }
    await wait(backoffMs(failures++));
  }
}

async function boot(): Promise<void> {
  const origin = envOrigin();
  if (!origin) {
    process.stderr.write("[entry] no env to register with: MAKEFUN_ENV_URL is missing\n");
    process.exit(1);
  }
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, resolve);
  });
  const bound = server.address();
  if (!bound || typeof bound === "string") {
    throw new Error("Expected a TCP address after opening the application listener");
  }
  process.stdout.write(`[entry] listening on port ${bound.port}\n`);
  await registerWithEnv(origin, bound.port);
  process.stdout.write("[entry] registered; waiting for the env to say done\n");
  await waitUntilDone(origin);
  server.close();
  process.stdout.write("[entry] done\n");
}

try {
  await boot();
  process.exit(0);
} catch (error) {
  process.stderr.write(`[entry] ${error instanceof Error ? error.message : String(error)}\n`);
  process.exit(1);
}
