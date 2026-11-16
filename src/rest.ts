// REST v3 (api.github.com shape): repos, refs, branches, commits, contents (backed by the
// same git object store as smart HTTP), labels, issues, comments and pull requests.
// Every reply is JSON with a non-empty X-GitHub-Request-Id (added by app.ts); 401/404/422
// bodies carry a string message; a malformed JSON body is a 400.

import type { IncomingMessage, ServerResponse } from 'node:http';
import { VIEWER } from './scenario.ts';
import { parseCommit } from './gitcore.ts';
import { FIXED_ISO, Store, type Issue, type Pull, type Repo } from './store.ts';

// ---------------------------------------------------------------- helpers

export function sendJson(res: ServerResponse, status: number, body: unknown, extraHeaders: Readonly<Record<string, string>> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(text), ...extraHeaders });
  res.end(text);
}

export function sendNoBody(res: ServerResponse, status: number): void {
  res.writeHead(status);
  res.end();
}

function message(text: string): { message: string } {
  return { message: text };
}

function parseJsonBody(body: Buffer): { ok: true; value: unknown } | { ok: false } {
  if (body.length === 0) return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(body.toString('utf8')) as unknown };
  } catch {
    return { ok: false };
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asString(value: unknown): string | null {
  return typeof value === 'string' ? value : null;
}

function parseAuthorLine(line: string): { name: string; email: string; date: string } {
  const match = /^(.*?) <([^>]*)> (\d+) ([+-]\d{4})$/.exec(line);
  if (match === null) return { name: line, email: '', date: FIXED_ISO };
  return { name: match[1] as string, email: match[2] as string, date: new Date(Number(match[3]) * 1000).toISOString() };
}

// ---------------------------------------------------------------- serializers

export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: Buffer.from(`Repository:${repo.owner}/${repo.name}`, 'utf8').toString('base64'),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    private: repo.isPrivate,
    owner: { login: repo.owner, id: VIEWER.id, type: 'User' },
    description: repo.description,
    fork: false,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    pushed_at: FIXED_ISO,
    homepage: null,
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    language: null,
    has_issues: true,
    has_projects: true,
    has_downloads: true,
    has_wiki: true,
    has_pages: false,
    forks_count: 0,
    open_issues_count: repo.issues.filter((issue) => issue.state === 'open').length,
    license: null,
    allow_forking: true,
    is_template: false,
    topics: [],
    visibility: repo.isPrivate ? 'private' : 'public',
    default_branch: repo.defaultBranch,
  };
}

function userJson(): Record<string, unknown> {
  return {
    login: VIEWER.login,
    id: VIEWER.id,
    node_id: Buffer.from(`User:${VIEWER.login}`, 'utf8').toString('base64'),
    avatar_url: '',
    type: 'User',
    site_admin: false,
    name: VIEWER.name,
    company: null,
    blog: '',
    location: '',
    email: VIEWER.email,
    hireable: null,
    bio: null,
    public_repos: 0,
    public_gists: 0,
    followers: 0,
    following: 0,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  };
}

function labelJson(repo: Repo, name: string): Record<string, unknown> | null {
  const label = repo.labels.get(name);
  if (label === undefined) return null;
  return {
    id: repo.id * 1000 + [...repo.labels.keys()].indexOf(name) + 1,
    node_id: Buffer.from(`Label:${repo.owner}/${repo.name}/${name}`, 'utf8').toString('base64'),
    url: `/api/v3/repos/${repo.owner}/${repo.name}/labels/${encodeURIComponent(name)}`,
    name: label.name,
    color: label.color,
    description: label.description,
    default: false,
  };
}

export function issueJson(repo: Repo, issue: Issue, asPull?: Pull): Record<string, unknown> {
  const out: Record<string, unknown> = {
    id: issue.id,
    node_id: Buffer.from(`Issue:${repo.owner}/${repo.name}/${issue.number}`, 'utf8').toString('base64'),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    locked: false,
    labels: issue.labels.map((name) => labelJson(repo, name)).filter((label) => label !== null),
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    assignee: null,
    assignees: [],
    milestone: null,
    comments: issue.comments.length,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    closed_at: issue.state === 'closed' ? FIXED_ISO : null,
    author_association: 'OWNER',
  };
  if (asPull !== undefined) {
    out.pull_request = { url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${asPull.number}`, html_url: '', diff_url: '', patch_url: '' };
  }
  return out;
}

export function pullJson(store: Store, repo: Repo, pull: Pull): Record<string, unknown> {
  const mergeable = !pull.merged && pull.state === 'open' ? !store.pullConflicts(repo, pull) : false;
  return {
    id: pull.id,
    node_id: Buffer.from(`PullRequest:${repo.owner}/${repo.name}/${pull.number}`, 'utf8').toString('base64'),
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    head: {
      ref: pull.headRef,
      sha: pull.headSha,
      label: `${repo.owner}:${pull.headRef}`,
      user: { login: repo.owner, id: VIEWER.id, type: 'User' },
      repo: repoJson(repo),
    },
    base: {
      ref: pull.baseRef,
      sha: pull.baseSha,
      label: `${repo.owner}:${pull.baseRef}`,
      user: { login: repo.owner, id: VIEWER.id, type: 'User' },
      repo: repoJson(repo),
    },
    merged: pull.merged,
    mergeable,
    mergeable_state: pull.merged ? 'clean' : mergeable ? 'clean' : 'dirty',
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    draft: false,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    closed_at: pull.state === 'closed' ? FIXED_ISO : null,
  };
}

export function commentJson(repo: Repo, issue: Issue, comment: { id: number; body: string }): Record<string, unknown> {
  return {
    id: comment.id,
    node_id: Buffer.from(`IssueComment:${repo.owner}/${repo.name}/${issue.number}/${comment.id}`, 'utf8').toString('base64'),
    body: comment.body,
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  };
}

// ---------------------------------------------------------------- pagination

function pageParams(query: URLSearchParams): { page: number; perPage: number } {
  const page = Math.max(1, Number(query.get('page') ?? '1') || 1);
  const perPage = Math.min(100, Math.max(1, Number(query.get('per_page') ?? '30') || 30));
  return { page, perPage };
}

function linkHeader(origin: string, path: string, query: URLSearchParams, page: number, perPage: number, total: number): string | null {
  const lastPage = Math.max(1, Math.ceil(total / perPage));
  if (lastPage <= 1 && page === 1) return null;
  const build = (p: number): string => {
    const q = new URLSearchParams(query);
    q.set('page', String(p));
    q.set('per_page', String(perPage));
    return `${origin}${path}?${q.toString()}`;
  };
  const parts: string[] = [];
  if (page < lastPage) parts.push(`<${build(page + 1)}>; rel="next"`);
  if (page > 1) parts.push(`<${build(page - 1)}>; rel="prev"`);
  parts.push(`<${build(1)}>; rel="first"`);
  parts.push(`<${build(lastPage)}>; rel="last"`);
  return parts.join(', ');
}

function paginate<T>(items: readonly T[], page: number, perPage: number): T[] {
  return items.slice((page - 1) * perPage, page * perPage);
}

// ---------------------------------------------------------------- router

export function handleRest(req: IncomingMessage, res: ServerResponse, store: Store, body: Buffer): void {
  const url = new URL(req.url ?? '/', 'http://entry');
  const query = url.searchParams;
  const segments = url.pathname.split('/').filter((part) => part !== '').slice(2).map((part) => {
    try {
      return decodeURIComponent(part);
    } catch {
      return part;
    }
  });
  const method = req.method ?? 'GET';
  const origin = `http://${req.headers.host ?? '127.0.0.1'}`;
  const path = url.pathname;

  const needRepo = (owner: string, name: string): Repo | null => {
    const repo = store.getRepo(owner, name);
    if (repo === undefined) {
      sendJson(res, 404, message('Not Found'));
      return null;
    }
    return repo;
  };

  // GET /api/v3/ — the API root.
  if (segments.length === 0) {
    if (method !== 'GET') return sendJson(res, 404, message('Not Found'));
    return sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
  }

  // GET /api/v3/meta
  if (segments.length === 1 && segments[0] === 'meta') {
    if (method !== 'GET') return sendJson(res, 404, message('Not Found'));
    return sendJson(res, 200, { installed_version: '1.0.0' });
  }

  // GET /api/v3/user
  if (segments.length === 1 && segments[0] === 'user') {
    if (method !== 'GET') return sendJson(res, 404, message('Not Found'));
    return sendJson(res, 200, userJson());
  }

  // /api/v3/user/repos
  if (segments.length === 2 && segments[0] === 'user' && segments[1] === 'repos') {
    if (method === 'GET') {
      const { page, perPage } = pageParams(query);
      const all = store.listRepos(VIEWER.login);
      const items = paginate(all, page, perPage).map((repo) => repoJson(repo));
      const headers: Record<string, string> = {};
      const link = linkHeader(origin, path, query, page, perPage, all.length);
      if (link !== null) headers.link = link;
      return sendJson(res, 200, items, headers);
    }
    if (method === 'POST') {
      const parsed = parseJsonBody(body);
      if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
      if (!isRecord(parsed.value)) return sendJson(res, 422, message('Validation Failed'));
      const name = asString(parsed.value.name);
      if (name === null || name.trim() === '' || /[/\s]/.test(name)) return sendJson(res, 422, message('name is required'));
      if (store.getRepo(VIEWER.login, name) !== undefined) {
        return sendJson(res, 422, message('Repository already exists'));
      }
      const description = asString(parsed.value.description) ?? '';
      const isPrivate = parsed.value.private === true;
      const repo = store.createRepo(VIEWER.login, name, description, isPrivate);
      return sendJson(res, 201, repoJson(repo));
    }
    return sendJson(res, 404, message('Not Found'));
  }

  // /api/v3/repos/{owner}/{name}[...]
  if (segments.length >= 3 && segments[0] === 'repos') {
    const owner = segments[1] as string;
    const name = segments[2] as string;
    const rest = segments.slice(3);

    // The repo itself.
    if (rest.length === 0) {
      if (method === 'GET') {
        const repo = needRepo(owner, name);
        if (repo === null) return;
        return sendJson(res, 200, repoJson(repo));
      }
      if (method === 'PATCH') {
        const repo = needRepo(owner, name);
        if (repo === null) return;
        const parsed = parseJsonBody(body);
        if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
        if (isRecord(parsed.value)) {
          if (typeof parsed.value.description === 'string') repo.description = parsed.value.description;
          if (typeof parsed.value.private === 'boolean') repo.isPrivate = parsed.value.private;
        }
        return sendJson(res, 200, repoJson(repo));
      }
      if (method === 'DELETE') {
        const repo = needRepo(owner, name);
        if (repo === null) return;
        store.deleteRepo(owner, name);
        return sendNoBody(res, 204);
      }
      return sendJson(res, 404, message('Not Found'));
    }

    // git/refs
    if (rest[0] === 'git' && rest[1] === 'refs' && rest.length === 2 && method === 'POST') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      const parsed = parseJsonBody(body);
      if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
      if (!isRecord(parsed.value)) return sendJson(res, 422, message('Validation Failed'));
      const ref = asString(parsed.value.ref);
      const sha = asString(parsed.value.sha);
      if (ref === null || !ref.startsWith('refs/') || sha === null || !/^[0-9a-f]{40}$/.test(sha)) {
        return sendJson(res, 422, message('Invalid ref or sha'));
      }
      if (!repo.objects.has(sha)) return sendJson(res, 422, message('Invalid SHA'));
      if (repo.refs.has(ref)) return sendJson(res, 422, message('Reference already exists'));
      repo.refs.set(ref, sha);
      const type = repo.objects.get(sha)?.type ?? 'commit';
      return sendJson(res, 201, { ref, node_id: '', object: { sha, type, url: '' }, url: '' });
    }
    if (rest[0] === 'git' && rest[1] === 'refs' && rest[2] === 'heads' && rest.length >= 4 && method === 'GET') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      const branch = rest.slice(3).join('/');
      const sha = repo.refs.get(`refs/heads/${branch}`);
      if (sha === undefined) return sendJson(res, 404, message('Not Found'));
      const type = repo.objects.get(sha)?.type ?? 'commit';
      return sendJson(res, 200, { ref: `refs/heads/${branch}`, node_id: '', object: { sha, type, url: '' }, url: '' });
    }

    // branches
    if (rest[0] === 'branches' && rest.length === 1 && method === 'GET') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      return sendJson(res, 200, store.branches(repo).map((branch) => ({ name: branch.name, commit: { sha: branch.sha, url: '' }, protected: false })));
    }
    if (rest[0] === 'branches' && rest.length >= 2 && method === 'GET') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      const branch = rest.slice(1).join('/');
      const sha = repo.refs.get(`refs/heads/${branch}`);
      if (sha === undefined) return sendJson(res, 404, message('Not Found'));
      return sendJson(res, 200, { name: branch, commit: { sha, url: '' }, protected: false });
    }

    // commits
    if (rest[0] === 'commits' && rest.length === 1 && method === 'GET') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      const ref = query.get('sha') ?? repo.defaultBranch;
      const commits = store.listCommits(repo, ref);
      if (commits.length === 0 && store.resolveRef(repo, ref) === null && !repo.objects.has(ref)) {
        return sendJson(res, 404, message('No commit found for SHA'));
      }
      return sendJson(res, 200, commits.map(({ sha, commit }) => {
        const author = parseAuthorLine(commit.author);
        const committer = parseAuthorLine(commit.committer);
        return {
          sha,
          commit: {
            message: commit.message,
            author: { name: author.name, email: author.email, date: author.date },
            committer: { name: committer.name, email: committer.email, date: committer.date },
          },
          author: { login: VIEWER.login },
          committer: { login: VIEWER.login },
          parents: commit.parents.map((parent) => ({ sha: parent })),
        };
      }));
    }

    // contents
    if (rest[0] === 'contents') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      const path = rest.slice(1).join('/');
      if (method === 'GET') {
        const ref = query.get('ref') ?? repo.defaultBranch;
        const read = store.readPath(repo, ref, path);
        if (read === null) return sendJson(res, 404, message('Not Found'));
        if (read.kind === 'file') {
          return sendJson(res, 200, {
            type: 'file',
            encoding: 'base64',
            size: read.data.length,
            name: path.split('/').pop() ?? path,
            path,
            content: read.data.toString('base64'),
            sha: read.blobOid,
          });
        }
        return sendJson(res, 200, read.entries.map((entry) => ({
          type: entry.mode === '40000' ? 'dir' : 'file',
          size: entry.mode === '40000' ? 0 : (repo.objects.get(entry.oid)?.data.length ?? 0),
          name: entry.name,
          path: path === '' ? entry.name : `${path}/${entry.name}`,
          sha: entry.oid,
        })));
      }
      if (method === 'PUT') {
        const parsed = parseJsonBody(body);
        if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
        if (!isRecord(parsed.value)) return sendJson(res, 422, message('Validation Failed'));
        const messageText = asString(parsed.value.message) ?? 'Update file';
        const contentB64 = asString(parsed.value.content);
        if (contentB64 === null) return sendJson(res, 422, message('content is required'));
        let content: Buffer;
        try {
          content = Buffer.from(contentB64, 'base64');
        } catch {
          return sendJson(res, 422, message('content is not valid base64'));
        }
        const branch = asString(parsed.value.branch) ?? repo.defaultBranch;
        const sha = asString(parsed.value.sha);
        const ref = `refs/heads/${branch}`;
        const tip = repo.refs.get(ref) ?? (branch === repo.defaultBranch ? null : repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null);
        const baseTree = tip === null ? null : parseCommit((repo.objects.get(tip) as { data: Buffer }).data).tree;
        const flat = store.flattenTree(repo, baseTree);
        const existing = flat.get(path);
        if (existing !== undefined) {
          if (sha === null) return sendJson(res, 422, message('sha is required to update a file'));
          if (sha !== existing.oid) return sendJson(res, 409, message('sha does not match the current blob'));
        } else if (sha !== null) {
          return sendJson(res, 422, message('sha does not match any existing file'));
        }
        const commitOid = store.commitFile(repo, branch, path, content, messageText);
        const blobOid = store.putObject(repo, 'blob', content);
        const isCreate = existing === undefined;
        return sendJson(res, isCreate ? 201 : 200, {
          content: {
            name: path.split('/').pop() ?? path,
            path,
            sha: blobOid,
            size: content.length,
            type: 'file',
            encoding: 'base64',
            content: content.toString('base64'),
          },
          commit: { sha: commitOid, message: messageText },
        });
      }
      if (method === 'DELETE') {
        const parsed = parseJsonBody(body);
        if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
        if (!isRecord(parsed.value)) return sendJson(res, 422, message('Validation Failed'));
        const messageText = asString(parsed.value.message) ?? 'Delete file';
        const sha = asString(parsed.value.sha);
        const branch = asString(parsed.value.branch) ?? repo.defaultBranch;
        const ref = `refs/heads/${branch}`;
        const tip = repo.refs.get(ref) ?? null;
        if (tip === null) return sendJson(res, 404, message('Not Found'));
        const baseTree = parseCommit((repo.objects.get(tip) as { data: Buffer }).data).tree;
        const flat = store.flattenTree(repo, baseTree);
        const existing = flat.get(path);
        if (existing === undefined) return sendJson(res, 404, message('Not Found'));
        if (sha !== null && sha !== existing.oid) return sendJson(res, 409, message('sha does not match the current blob'));
        const commitOid = store.deleteFile(repo, branch, path, messageText);
        if (commitOid === null) return sendJson(res, 404, message('Not Found'));
        return sendJson(res, 200, { commit: { sha: commitOid, message: messageText }, content: null });
      }
      return sendJson(res, 404, message('Not Found'));
    }

    // labels
    if (rest[0] === 'labels') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      if (rest.length === 1 && method === 'GET') {
        return sendJson(res, 200, [...repo.labels.keys()].map((labelName) => labelJson(repo, labelName)));
      }
      if (rest.length === 1 && method === 'POST') {
        const parsed = parseJsonBody(body);
        if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
        if (!isRecord(parsed.value)) return sendJson(res, 422, message('Validation Failed'));
        const labelName = asString(parsed.value.name);
        if (labelName === null || labelName.trim() === '') return sendJson(res, 422, message('name is required'));
        if (repo.labels.has(labelName)) return sendJson(res, 422, message('Label already exists'));
        const color = asString(parsed.value.color) ?? 'ededed';
        const description = asString(parsed.value.description) ?? '';
        repo.labels.set(labelName, { name: labelName, color, description });
        return sendJson(res, 201, labelJson(repo, labelName));
      }
      return sendJson(res, 404, message('Not Found'));
    }

    // issues
    if (rest[0] === 'issues') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      if (rest.length === 1 && method === 'GET') {
        const state = query.get('state') ?? 'open';
        const sort = query.get('sort') ?? 'created';
        const direction = query.get('direction') ?? 'desc';
        let items = [...repo.issues];
        if (state !== 'all') items = items.filter((issue) => issue.state === state);
        if (sort === 'created') {
          items.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
        }
        const { page, perPage } = pageParams(query);
        return sendJson(res, 200, paginate(items, page, perPage).map((issue) => issueJson(repo, issue)));
      }
      if (rest.length === 1 && method === 'POST') {
        const parsed = parseJsonBody(body);
        if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
        if (!isRecord(parsed.value)) return sendJson(res, 422, message('Validation Failed'));
        const title = asString(parsed.value.title);
        if (title === null || title.trim() === '') return sendJson(res, 422, message('title is required'));
        const bodyText = asString(parsed.value.body) ?? '';
        const labelNames = Array.isArray(parsed.value.labels)
          ? (parsed.value.labels as unknown[]).map((label) => (typeof label === 'string' ? label : (isRecord(label) ? asString(label.name) : null))).filter((label): label is string => label !== null)
          : [];
        for (const labelName of labelNames) {
          if (!repo.labels.has(labelName)) return sendJson(res, 422, message(`Label '${labelName}' does not exist`));
        }
        const issue = store.createIssue(repo, title, bodyText, labelNames);
        return sendJson(res, 201, issueJson(repo, issue));
      }
      if (rest.length === 2) {
        const number = Number(rest[1]);
        if (!Number.isSafeInteger(number) || number < 1) return sendJson(res, 404, message('Not Found'));
        if (method === 'GET') {
          const issue = store.findIssue(repo, number);
          if (issue !== undefined) return sendJson(res, 200, issueJson(repo, issue));
          const pull = store.findPull(repo, number);
          if (pull !== undefined) {
            // A pull request read through the issues view (GitHub returns it with a pull_request key).
            const pseudo: Issue = { id: pull.id, number: pull.number, title: pull.title, body: pull.body, state: pull.state, labels: [], comments: [] };
            return sendJson(res, 200, issueJson(repo, pseudo, pull));
          }
          return sendJson(res, 404, message('Not Found'));
        }
        if (method === 'PATCH') {
          const issue = store.findIssue(repo, number);
          if (issue === undefined) return sendJson(res, 404, message('Not Found'));
          const parsed = parseJsonBody(body);
          if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
          if (isRecord(parsed.value)) {
            if (parsed.value.state === 'open' || parsed.value.state === 'closed') issue.state = parsed.value.state;
            if (typeof parsed.value.title === 'string') issue.title = parsed.value.title;
            if (typeof parsed.value.body === 'string') issue.body = parsed.value.body;
          }
          return sendJson(res, 200, issueJson(repo, issue));
        }
        return sendJson(res, 404, message('Not Found'));
      }
      if (rest.length === 3 && rest[2] === 'comments') {
        const number = Number(rest[1]);
        if (!Number.isSafeInteger(number) || number < 1) return sendJson(res, 404, message('Not Found'));
        const issue = store.findIssue(repo, number);
        if (issue === undefined) return sendJson(res, 404, message('Not Found'));
        if (method === 'GET') {
          return sendJson(res, 200, issue.comments.map((comment) => commentJson(repo, issue, comment)));
        }
        if (method === 'POST') {
          const parsed = parseJsonBody(body);
          if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
          if (!isRecord(parsed.value)) return sendJson(res, 422, message('Validation Failed'));
          const text = asString(parsed.value.body);
          if (text === null || text.trim() === '') return sendJson(res, 422, message('body is required'));
          const comment = { id: repo.nextCommentId++, body: text };
          issue.comments.push(comment);
          return sendJson(res, 201, commentJson(repo, issue, comment));
        }
        return sendJson(res, 404, message('Not Found'));
      }
      return sendJson(res, 404, message('Not Found'));
    }

    // pulls
    if (rest[0] === 'pulls') {
      const repo = needRepo(owner, name);
      if (repo === null) return;
      if (rest.length === 1 && method === 'GET') {
        const state = query.get('state') ?? 'open';
        let items = [...repo.pulls];
        if (state !== 'all') items = items.filter((pull) => pull.state === state);
        items.sort((a, b) => b.number - a.number);
        const { page, perPage } = pageParams(query);
        return sendJson(res, 200, paginate(items, page, perPage).map((pull) => pullJson(store, repo, pull)));
      }
      if (rest.length === 1 && method === 'POST') {
        const parsed = parseJsonBody(body);
        if (!parsed.ok) return sendJson(res, 400, message('malformed JSON body'));
        if (!isRecord(parsed.value)) return sendJson(res, 422, message('Validation Failed'));
        const title = asString(parsed.value.title);
        let head = asString(parsed.value.head);
        let base = asString(parsed.value.base);
        if (title === null || head === null || base === null) return sendJson(res, 422, message('title, head and base are required'));
        head = head.includes(':') ? (head.split(':').pop() as string) : head;
        base = base.includes(':') ? (base.split(':').pop() as string) : base;
        if (store.resolveRef(repo, head) === null) return sendJson(res, 422, message(`Head branch '${head}' not found`));
        if (store.resolveRef(repo, base) === null) return sendJson(res, 422, message(`Base branch '${base}' not found`));
        const duplicate = repo.pulls.find((pull) => pull.state === 'open' && pull.headRef === head && pull.baseRef === base);
        if (duplicate !== undefined) return sendJson(res, 422, message('A pull request already exists for these branches'));
        const pull = store.createPull(repo, title, asString(parsed.value.body) ?? '', head, base);
        if (pull === null) return sendJson(res, 422, message('could not create the pull request'));
        return sendJson(res, 201, pullJson(store, repo, pull));
      }
      if (rest.length === 2) {
        const number = Number(rest[1]);
        if (!Number.isSafeInteger(number) || number < 1) return sendJson(res, 404, message('Not Found'));
        const pull = store.findPull(repo, number);
        if (pull === undefined) return sendJson(res, 404, message('Not Found'));
        if (method === 'GET') return sendJson(res, 200, pullJson(store, repo, pull));
        return sendJson(res, 404, message('Not Found'));
      }
      if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
        const number = Number(rest[1]);
        if (!Number.isSafeInteger(number) || number < 1) return sendJson(res, 404, message('Not Found'));
        const pull = store.findPull(repo, number);
        if (pull === undefined) return sendJson(res, 404, message('Not Found'));
        const baseTip = store.resolveRef(repo, pull.baseRef);
        const headTip = store.resolveRef(repo, pull.headRef);
        const baseTree = baseTip === null ? null : parseCommit((repo.objects.get(baseTip) as { data: Buffer }).data).tree;
        const headTree = headTip === null ? null : parseCommit((repo.objects.get(headTip) as { data: Buffer }).data).tree;
        const files = store.diffTrees(repo, baseTree, headTree);
        return sendJson(res, 200, files.map((file) => ({
          filename: file.filename,
          status: file.status,
          additions: file.additions,
          deletions: file.deletions,
          changes: file.additions + file.deletions,
        })));
      }
      if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
        const number = Number(rest[1]);
        if (!Number.isSafeInteger(number) || number < 1) return sendJson(res, 404, message('Not Found'));
        const pull = store.findPull(repo, number);
        if (pull === undefined) return sendJson(res, 404, message('Not Found'));
        if (pull.state !== 'open' || pull.merged) return sendJson(res, 405, message('Pull request is not mergeable'));
        if (store.pullConflicts(repo, pull)) return sendJson(res, 405, message('Merge conflict'));
        const mergeOid = store.mergePull(repo, pull);
        if (mergeOid === null) return sendJson(res, 405, message('Merge conflict'));
        return sendJson(res, 200, { merged: true, sha: mergeOid, message: 'Pull request successfully merged' });
      }
      return sendJson(res, 404, message('Not Found'));
    }
  }

  // Nothing matched: GitHub's JSON 404.
  sendJson(res, 404, message('Not Found'));
}
