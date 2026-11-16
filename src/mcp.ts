// MCP (Model Context Protocol, streamable HTTP on POST /mcp): the modern server/discover
// era first, with the legacy initialize handshake as a fallback. Plain JSON-RPC responses
// (no SSE needed — the client accepts either). Every tool result carries both a text block
// (JSON) and structuredContent; effects are the same store objects REST serves.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { VIEWER } from './scenario.ts';
import { Store } from './store.ts';
import { issueJson, pullJson, repoJson, sendJson } from './rest.ts';

const SERVER_NAME = 'github-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  readonly name: string;
  readonly description: string;
  readonly inputSchema: { type: 'object'; properties: Record<string, Record<string, unknown>>; required: string[] };
}

// One constant tool list: tools/list must be byte-identical across calls.
const TOOLS: readonly ToolDef[] = Object.freeze([
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login (defaults to the authenticated user)' },
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state: open, closed or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
  {
    name: 'list_repos',
    description: 'List the repositories of the authenticated user.',
    inputSchema: { type: 'object', properties: {}, required: [] },
  },
  {
    name: 'get_issue',
    description: 'Read one issue by owner, repository and number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        issue_number: { type: 'integer', description: 'Issue number' },
      },
      required: ['owner', 'repo', 'issue_number'],
    },
  },
]);

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

interface RpcError {
  readonly code: number;
  readonly message: string;
}

type ToolOutcome =
  | { ok: true; result: unknown }
  | { ok: false; error: RpcError }
  | { ok: false; toolError: string };

function callTool(store: Store, name: string, args: Record<string, unknown>): ToolOutcome {
  const tool = TOOLS.find((candidate) => candidate.name === name);
  if (tool === undefined) return { ok: false, error: { code: -32602, message: `Unknown tool: ${name}` } };
  for (const required of tool.inputSchema.required) {
    if (args[required] === undefined || args[required] === null || args[required] === '') {
      return { ok: false, error: { code: -32602, message: `Missing required argument: ${required}` } };
    }
  }
  const str = (key: string): string | null => (typeof args[key] === 'string' ? (args[key] as string) : null);
  const int = (key: string): number | null => (typeof args[key] === 'number' && Number.isSafeInteger(args[key]) ? (args[key] as number) : null);

  const repoOf = (): { repo: ReturnType<Store['getRepo']>; owner: string; name: string } | null => {
    const owner = str('owner') ?? VIEWER.login;
    const repoName = str('repo');
    if (repoName === null) return null;
    const repo = store.getRepo(owner, repoName);
    return repo === undefined ? null : { repo, owner, name: repoName };
  };

  switch (name) {
    case 'create_repo': {
      const repoName = str('name') as string;
      if (store.getRepo(VIEWER.login, repoName) !== undefined) {
        return { ok: false, toolError: `Repository ${VIEWER.login}/${repoName} already exists` };
      }
      const repo = store.createRepo(VIEWER.login, repoName, str('description') ?? '', args.private === true);
      return { ok: true, result: repoJson(repo) };
    }
    case 'list_repos': {
      return { ok: true, result: { repositories: store.listRepos(VIEWER.login).map((repo) => repoJson(repo)) } };
    }
    case 'get_repo': {
      const found = repoOf();
      if (found === null) return { ok: false, toolError: 'Repository not found' };
      return { ok: true, result: repoJson(found.repo) };
    }
    case 'create_issue': {
      const found = repoOf();
      if (found === null) return { ok: false, toolError: 'Repository not found' };
      const issue = store.createIssue(found.repo, str('title') as string, str('body') ?? '', []);
      return { ok: true, result: issueJson(found.repo, issue) };
    }
    case 'get_issue': {
      const found = repoOf();
      const number = int('issue_number');
      if (found === null || number === null) return { ok: false, toolError: 'Repository or issue not found' };
      const issue = store.findIssue(found.repo, number);
      if (issue === undefined) return { ok: false, toolError: `Issue #${number} not found` };
      return { ok: true, result: issueJson(found.repo, issue) };
    }
    case 'list_issues': {
      const found = repoOf();
      if (found === null) return { ok: false, toolError: 'Repository not found' };
      const state = str('state') ?? 'open';
      const items = state === 'all' ? found.repo.issues : found.repo.issues.filter((issue) => issue.state === state);
      return { ok: true, result: { issues: items.map((issue) => issueJson(found.repo, issue)) } };
    }
    case 'create_pull_request': {
      const found = repoOf();
      if (found === null) return { ok: false, toolError: 'Repository not found' };
      const head = str('head') as string;
      const base = str('base') as string;
      if (store.resolveRef(found.repo, head) === null) return { ok: false, toolError: `Head branch '${head}' not found` };
      if (store.resolveRef(found.repo, base) === null) return { ok: false, toolError: `Base branch '${base}' not found` };
      const duplicate = found.repo.pulls.find((pull) => pull.state === 'open' && pull.headRef === head && pull.baseRef === base);
      const pull = duplicate ?? store.createPull(found.repo, str('title') as string, str('body') ?? '', head, base);
      if (pull === null) return { ok: false, toolError: 'could not create the pull request' };
      return { ok: true, result: pullJson(store, found.repo, pull) };
    }
    case 'merge_pull_request': {
      const found = repoOf();
      const number = int('pull_number');
      if (found === null || number === null) return { ok: false, toolError: 'Repository or pull request not found' };
      const pull = store.findPull(found.repo, number);
      if (pull === undefined) return { ok: false, toolError: `Pull request #${number} not found` };
      if (pull.state !== 'open' || pull.merged) return { ok: false, toolError: `Pull request #${number} is not open` };
      if (store.pullConflicts(found.repo, pull)) return { ok: false, toolError: 'Merge conflict' };
      const merged = store.mergePull(found.repo, pull);
      if (merged === null) return { ok: false, toolError: 'Merge conflict' };
      return { ok: true, result: { number: pull.number, merged: true, state: 'closed', merge_commit_sha: merged } };
    }
    default:
      return { ok: false, error: { code: -32602, message: `Unknown tool: ${name}` } };
  }
}

// ---------------------------------------------------------------- HTTP entry

function rpcResult(res: ServerResponse, id: unknown, result: unknown): void {
  sendJson(res, 200, { jsonrpc: '2.0', id: id ?? null, result });
}

function rpcError(res: ServerResponse, status: number, id: unknown, error: RpcError): void {
  sendJson(res, status, { jsonrpc: '2.0', id: id ?? null, error });
}

export function handleMcp(req: IncomingMessage, res: ServerResponse, store: Store, body: Buffer): void {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.toString('utf8')) as unknown;
  } catch {
    // A body that is not JSON: HTTP 400 with JSON-RPC -32700.
    return rpcError(res, 400, null, { code: -32700, message: 'Parse error' });
  }
  if (!isRecord(parsed) || typeof parsed.method !== 'string') {
    return rpcError(res, 400, isRecord(parsed) ? parsed.id : null, { code: -32600, message: 'Invalid Request' });
  }
  const id = parsed.id ?? null;
  const method = parsed.method as string;
  const params = isRecord(parsed.params) ? parsed.params : {};

  // A notification (no id): acknowledge with an empty 200 and do nothing.
  if (parsed.id === undefined) {
    res.writeHead(200);
    res.end();
    return;
  }

  switch (method) {
    case 'server/discover':
      return rpcResult(res, id, {
        supportedVersions: SUPPORTED_VERSIONS,
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
        _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
      });
    case 'initialize': {
      const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : null;
      const negotiated = offered !== null && SUPPORTED_VERSIONS.includes(offered) ? offered : '2026-07-28';
      return rpcResult(res, id, {
        protocolVersion: negotiated,
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      });
    }
    case 'ping':
      return rpcResult(res, id, {});
    case 'tools/list':
      return rpcResult(res, id, { tools: TOOLS });
    case 'tools/call': {
      const toolName = typeof params.name === 'string' ? params.name : null;
      const args = isRecord(params.arguments) ? params.arguments : {};
      if (toolName === null) {
        return rpcError(res, 200, id, { code: -32602, message: 'tools/call needs a tool name' });
      }
      const outcome = callTool(store, toolName, args);
      if (!outcome.ok && 'error' in outcome) {
        return rpcError(res, 200, id, outcome.error);
      }
      if (!outcome.ok) {
        return rpcResult(res, id, {
          content: [{ type: 'text', text: outcome.toolError }],
          structuredContent: { error: outcome.toolError },
          isError: true,
        });
      }
      return rpcResult(res, id, {
        content: [{ type: 'text', text: JSON.stringify(outcome.result) }],
        structuredContent: outcome.result,
        isError: false,
      });
    }
    default:
      return rpcError(res, 200, id, { code: -32601, message: `Method not found: ${method}` });
  }
}
