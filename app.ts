// The server: one in-memory forge per process. Routing, auth and the request-id header live
// here; the git, REST, GraphQL and MCP surfaces live in ./src/.
//
// Auth: every route except GET /healthz requires `Authorization: Bearer <token>`. When
// MAKEFUN_SHOP_TOKEN is set (a trial), the token must equal it; when it is unset (local
// practice), any non-empty bearer token is accepted and a missing one is a 401.
// Every reply carries a non-empty X-GitHub-Request-Id (an incrementing counter — no
// randomness, so a live trial and its replay answer identically).

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleGraphQL } from './src/graphql.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack, handleUploadPackV0 } from './src/githttp.ts';
import { handleMcp } from './src/mcp.ts';
import { handleRest, sendJson } from './src/rest.ts';
import { Store } from './src/store.ts';

const SHOP_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const MAX_BODY_BYTES = 64 * 1024 * 1024;

let requestCounter = 0;

function authorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization ?? '';
  if (!header.startsWith('Bearer ')) return false;
  const token = header.slice('Bearer '.length);
  if (token === '') return false;
  if (SHOP_TOKEN !== '') return token === SHOP_TOKEN;
  return true;
}

function decodeSegment(part: string): string {
  try {
    return decodeURIComponent(part);
  } catch {
    return part;
  }
}

/** Read the request body fully (up to MAX_BODY_BYTES); empty for GET/HEAD. */
function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise<Buffer>((resolve, reject) => {
    if (req.method === 'GET' || req.method === 'HEAD') {
      resolve(Buffer.alloc(0));
      return;
    }
    const chunks: Buffer[] = [];
    let total = 0;
    let done = false;
    req.on('data', (chunk: Buffer) => {
      if (done) return;
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        done = true;
        reject(new Error('request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (!done) resolve(Buffer.concat(chunks));
    });
    req.on('error', (err) => {
      if (!done) {
        done = true;
        reject(err);
      }
    });
  });
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  const store = new Store();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    requestCounter += 1;
    res.setHeader('x-github-request-id', `req-${requestCounter}`);
    const method = req.method ?? 'GET';
    let path = '/';
    try {
      path = new URL(req.url ?? '/', 'http://entry').pathname;
    } catch {
      path = '/';
    }

    // The env's boot check calls this twice: 200 {"ok":true}. No auth.
    if (method === 'GET' && path === '/healthz') {
      sendJson(res, 200, { ok: true });
      return;
    }

    if (!authorized(req)) {
      sendJson(res, 401, { message: 'Bad credentials' });
      return;
    }

    void readBody(req).then(
      (body) => {
        try {
          route(req, res, store, method, path, body);
        } catch (err) {
          // A handler must never take the server down with it.
          process.stderr.write(`[entry] handler error: ${err instanceof Error ? err.message : String(err)}\n`);
          if (!res.writableEnded) sendJson(res, 500, { message: 'Internal Server Error' });
        }
      },
      () => {
        if (!res.writableEnded) sendJson(res, 413, { message: 'request body too large' });
      },
    );
  });
}

function route(req: IncomingMessage, res: ServerResponse, store: Store, method: string, path: string, body: Buffer): void {
  const segments = path.split('/').filter((part) => part !== '').map(decodeSegment);

  // MCP: streamable HTTP on POST /mcp (DELETE closes a session; answer 200).
  if (path === '/mcp') {
    if (method === 'POST') {
      handleMcp(req, res, store, body);
      return;
    }
    if (method === 'DELETE') {
      res.writeHead(200);
      res.end();
      return;
    }
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }

  // GraphQL v4.
  if (path === '/api/graphql' || path === '/api/graphql/') {
    if (method === 'POST') {
      handleGraphQL(req, res, store, body);
      return;
    }
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }

  // Git smart HTTP: /<owner>/<repo>.git/<service-path>.
  if (
    segments.length >= 3 &&
    segments[1] !== undefined &&
    segments[1].endsWith('.git') &&
    (segments[2] === 'info' || segments[2] === 'git-upload-pack' || segments[2] === 'git-receive-pack')
  ) {
    const owner = segments[0] as string;
    const repoName = (segments[1] as string).slice(0, -'.git'.length);
    if (method === 'GET' && segments[2] === 'info' && segments[3] === 'refs') {
      handleInfoRefs(req, res, store, owner, repoName);
      return;
    }
    if (method === 'POST' && segments[2] === 'git-upload-pack') {
      if ((req.headers['git-protocol'] ?? '') === 'version=2') handleUploadPack(req, res, store, owner, repoName, body);
      else handleUploadPackV0(req, res, store, owner, repoName, body);
      return;
    }
    if (method === 'POST' && segments[2] === 'git-receive-pack') {
      handleReceivePack(req, res, store, owner, repoName, body);
      return;
    }
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }

  // REST v3: /api/v3 and everything under it.
  if (path === '/api/v3' || path === '/api/v3/' || path.startsWith('/api/v3/')) {
    handleRest(req, res, store, body);
    return;
  }

  // Everything else, every method: GitHub's 404 body.
  sendJson(res, 404, { message: 'Not Found' });
}
