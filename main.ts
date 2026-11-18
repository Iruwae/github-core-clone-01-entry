// Thin launcher: the forge itself is the Go binary beside this file. This
// process only starts it, learns its port from its stdout, tells the
// environment coordinator where it lives, and waits until the coordinator
// says the run is over — then stops the binary and exits.
//
// The coordinator protocol (unchanged): POST /register { port } until it
// answers 200 or 409, then poll GET /done until { done: true }. One request
// may take up to 30 s before it is dropped and retried.

import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const REQUEST_TIMEOUT_MS = 30_000;
const FIRST_BACKOFF_MS = 250;
const BACKOFF_CAP_MS = 2_000;
const BACKOFF_MAX_DOUBLINGS = 8;

function envOrigin(): string {
  return (process.env.MAKEFUN_ENV_URL ?? "").replace(/\/+$/, "");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// Backoff for attempt number `failures` (0-based): 250, 500, 1000, 2000, ...
function backoffMs(failures: number): number {
  const step = Math.min(BACKOFF_MAX_DOUBLINGS, failures);
  return Math.min(BACKOFF_CAP_MS, FIRST_BACKOFF_MS * 2 ** step);
}

// Start the Go binary and resolve with its child handle plus the port it
// printed. Rejects if the binary exits before printing the port.
function startForge(): Promise<{ child: ChildProcess; port: number }> {
  const binary = join(dirname(fileURLToPath(import.meta.url)), "forge");
  const child = spawn(binary, [], { stdio: ["ignore", "pipe", "inherit"] });
  return new Promise((resolve, reject) => {
    const lines = createInterface({ input: child.stdout! });
    const onLine = (line: string) => {
      const match = /\[entry\] listening on port (\d+)/.exec(line);
      if (match) {
        lines.close();
        resolve({ child, port: Number(match[1]) });
      }
    };
    lines.on("line", onLine);
    child.once("exit", (code) => {
      lines.close();
      reject(new Error(`forge exited before listening (code ${code})`));
    });
    child.once("error", (err) => {
      lines.close();
      reject(err);
    });
  });
}

async function coordinatorFetch(origin: string, path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(origin + path, { ...init, signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
}

async function registerPort(origin: string, port: number): Promise<void> {
  let failures = 0;
  for (;;) {
    let response: Response | undefined;
    try {
      response = await coordinatorFetch(origin, "/register", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ port }),
      });
    } catch {
      // The coordinator may still be starting; retry below.
    }
    if (response) {
      if (response.status === 200 || response.status === 409) return;
      if (response.status === 400 || response.status === 405) {
        throw new Error(`Registration rejected: HTTP ${response.status}: ${await response.text()}`);
      }
    }
    await sleep(backoffMs(failures++));
  }
}

async function waitForDone(origin: string): Promise<void> {
  let failures = 0;
  for (;;) {
    try {
      const response = await coordinatorFetch(origin, "/done");
      if (response.ok) {
        const payload: unknown = await response.json();
        if (typeof payload === "object" && payload !== null && "done" in payload && payload.done === true) return;
      }
    } catch {
      // A failed poll is no evidence the run finished; keep polling.
    }
    await sleep(backoffMs(failures++));
  }
}

async function main(): Promise<void> {
  const origin = envOrigin();
  if (!origin) {
    process.stderr.write("[entry] no env to register with: MAKEFUN_ENV_URL is missing\n");
    process.exit(1);
  }
  const { child, port } = await startForge();
  process.stdout.write(`[entry] listening on port ${port}\n`);
  try {
    await registerPort(origin, port);
    process.stdout.write("[entry] registered; waiting for the env to say done\n");
    await waitForDone(origin);
  } finally {
    child.kill("SIGTERM");
  }
  process.stdout.write("[entry] done\n");
}

try {
  await main();
  process.exit(0);
} catch (error) {
  process.stderr.write(`[entry] ${error instanceof Error ? error.message : String(error)}\n`);
  process.exit(1);
}
