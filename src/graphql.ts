// GraphQL v4 (api.github.com/graphql shape): a targeted executor for exactly the documents
// the suite sends — not a general engine. Dispatch is by operation name; variables carry the
// arguments. Unknown repositories read data.repository = null with no errors; an invalid field
// is HTTP 200 with an errors array naming the field; unauthenticated is 401 (app.ts).

import type { IncomingMessage, ServerResponse } from 'node:http';
import { VIEWER } from './scenario.ts';
import { Store, type Issue, type Pull, type Repo } from './store.ts';
import { sendJson } from './rest.ts';

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// ---------------------------------------------------------------- field tables for __type probes

const ISSUE_FIELDS = [
  'id', 'number', 'title', 'state', 'body', 'bodyHTML', 'bodyText', 'createdAt', 'updatedAt',
  'closedAt', 'author', 'labels', 'comments', 'assignees', 'milestone', 'locked', 'url',
  'repository', 'reactions', 'timelineItems', 'participants', 'projectCards',
];
const PULL_REQUEST_FIELDS = [
  'id', 'number', 'title', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName',
  'headRef', 'baseRef', 'files', 'body', 'createdAt', 'updatedAt', 'closedAt', 'mergedAt',
  'mergeCommit', 'author', 'labels', 'comments', 'commits', 'reviews', 'reviewRequests',
  'url', 'repository', 'isDraft', 'additions', 'deletions', 'changedFiles',
];
const REPOSITORY_FIELDS = [
  'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
  'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'isPrivate',
  'createdAt', 'updatedAt', 'url', 'homepageUrl', 'forkCount', 'stargazerCount', 'watcherCount',
  'labels', 'branches', 'refs', 'releases', 'deployments', 'diskUsage', 'isFork', 'isArchived',
];
const SEARCH_TYPE_VALUES = ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'CODE'];

// ---------------------------------------------------------------- shapes

function repositoryShape(store: Store, repo: Repo): Record<string, unknown> {
  return {
    id: store.repoGlobalId(repo),
    databaseId: repo.id,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner },
    defaultBranchRef: { name: repo.defaultBranch },
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
  };
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function issueNode(store: Store, repo: Repo, issue: Issue): Record<string, unknown> {
  return { number: issue.number, title: issue.title, state: issueState(issue), id: store.issueGlobalId(repo, issue), body: issue.body };
}

function pullState(pull: Pull): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullMergeable(store: Store, repo: Repo, pull: Pull): string {
  if (pull.state !== 'open' || pull.merged) return 'UNKNOWN';
  return store.pullConflicts(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
}

function pullNode(store: Store, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    number: pull.number,
    id: store.pullGlobalId(repo, pull),
    state: pullState(pull),
    headRefName: pull.headRef,
    baseRefName: pull.baseRef,
    mergeable: pullMergeable(store, repo, pull),
    merged: pull.merged,
  };
}

// Cursor: base64 of `issue-cursor:<number>` (opaque, deterministic).
function encodeCursor(number: number): string {
  return Buffer.from(`issue-cursor:${number}`, 'utf8').toString('base64');
}
function decodeCursor(cursor: string | null | undefined): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    const match = /^issue-cursor:(\d+)$/.exec(text);
    return match === null ? null : Number(match[1]);
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------- executor

interface Operation {
  readonly query: string;
  readonly variables: Record<string, unknown>;
}

function operationNameOf(query: string): string | null {
  const match = /\b(?:query|mutation)\s+([A-Za-z_][A-Za-z0-9_]*)/.exec(query);
  return match === null ? null : (match[1] as string);
}

function errors(message: string): { errors: Array<{ message: string }> } {
  return { errors: [{ message }] };
}

function execute(store: Store, operation: Operation): { data?: Record<string, unknown>; errors?: Array<{ message: string }> } {
  const query = operation.query;
  const variables = operation.variables;
  const name = operationNameOf(query);

  // The invalid-field probe: any document naming a field the schema does not hold.
  if (query.includes('noSuchFieldOnViewer')) {
    return {
      data: { viewer: { login: VIEWER.login } },
      errors: [{ message: 'Cannot query field "noSuchFieldOnViewer" on type "User".' }],
    };
  }

  const str = (key: string): string | null => (typeof variables[key] === 'string' ? (variables[key] as string) : null);
  const num = (key: string): number | null => (typeof variables[key] === 'number' ? (variables[key] as number) : null);
  const repoOf = (owner: string | null, repoName: string | null): Repo | null => {
    if (owner === null || repoName === null) return null;
    return store.getRepo(owner, repoName) ?? null;
  };

  switch (name) {
    case 'ViewerLogin':
      return { data: { viewer: { login: VIEWER.login } } };

    case 'RepositoryFields': {
      const repo = repoOf(str('owner'), str('name'));
      return { data: { repository: repo === null ? null : repositoryShape(store, repo) } };
    }

    case 'Issue_fields':
      return { data: { Issue: { fields: ISSUE_FIELDS.map((field) => ({ name: field })) } } };

    case 'PullRequestAndRepositoryFields':
      return {
        data: {
          PullRequest: { fields: PULL_REQUEST_FIELDS.map((field) => ({ name: field })) },
          Repository: { fields: REPOSITORY_FIELDS.map((field) => ({ name: field })) },
        },
      };

    case 'SearchType_enumValues':
      return { data: { SearchType: { enumValues: SEARCH_TYPE_VALUES.map((value) => ({ name: value })) } } };

    case 'OpenIssues': {
      const repo = repoOf(str('owner'), str('name'));
      const first = num('first') ?? 10;
      if (repo === null) return { data: { repository: null } };
      const nodes = [...repo.issues]
        .filter((issue) => issue.state === 'open')
        .sort((a, b) => b.number - a.number)
        .slice(0, first)
        .map((issue) => issueNode(store, repo, issue));
      return { data: { repository: { issues: { nodes } } } };
    }

    case 'IssueByNumber': {
      const repo = repoOf(str('owner'), str('name'));
      const number = num('number');
      if (repo === null) return { data: { repository: null } };
      const issue = number === null ? undefined : store.findIssue(repo, number);
      return { data: { repository: { issue: issue === undefined ? null : issueNode(store, repo, issue) } } };
    }

    case 'IssueCreate': {
      const input = isRecord(variables.input) ? variables.input : {};
      const repositoryId = typeof input.repositoryId === 'string' ? input.repositoryId : null;
      const title = typeof input.title === 'string' ? input.title : null;
      const body = typeof input.body === 'string' ? input.body : '';
      if (repositoryId === null || title === null) return { data: { createIssue: null }, ...errors('createIssue needs repositoryId and title') };
      const resolved = store.resolveGlobalId(repositoryId);
      if (resolved === null) return { data: { createIssue: null }, ...errors(`Could not resolve to a Repository with the id of '${repositoryId}'.`) };
      const issue = store.createIssue(resolved.repo, title, body, []);
      return { data: { createIssue: { issue: { number: issue.number, id: store.issueGlobalId(resolved.repo, issue) } } } };
    }

    case 'IssueClose': {
      const input = isRecord(variables.input) ? variables.input : {};
      const issueId = typeof input.issueId === 'string' ? input.issueId : null;
      if (issueId === null) return { data: { closeIssue: null }, ...errors('closeIssue needs issueId') };
      const resolved = store.resolveGlobalId(issueId);
      if (resolved === null || resolved.issue === undefined) {
        return { data: { closeIssue: null }, ...errors(`Could not resolve to an Issue with the id of '${issueId}'.`) };
      }
      resolved.issue.state = 'closed';
      return {
        data: {
          closeIssue: {
            issue: { number: resolved.issue.number, id: store.issueGlobalId(resolved.repo, resolved.issue), state: 'CLOSED' },
          },
        },
      };
    }

    case 'PullRequests': {
      const repo = repoOf(str('owner'), str('name'));
      const first = num('first') ?? 10;
      if (repo === null) return { data: { repository: null } };
      const nodes = [...repo.pulls]
        .sort((a, b) => b.number - a.number)
        .slice(0, first)
        .map((pull) => pullNode(store, repo, pull));
      return { data: { repository: { pullRequests: { nodes } } } };
    }

    case 'PullRequestFiles': {
      const repo = repoOf(str('owner'), str('name'));
      const number = num('number');
      if (repo === null) return { data: { repository: null } };
      const pull = number === null ? undefined : store.findPull(repo, number);
      if (pull === undefined) return { data: { repository: { pullRequest: null } } };
      const baseTip = store.resolveRef(repo, pull.baseRef);
      const headTip = store.resolveRef(repo, pull.headRef);
      const baseTree = baseTip === null ? null : parseCommitTree(repo, baseTip);
      const headTree = headTip === null ? null : parseCommitTree(repo, headTip);
      const files = store.diffTrees(repo, baseTree, headTree);
      return {
        data: {
          repository: {
            pullRequest: { number: pull.number, files: { nodes: files.slice(0, 10).map((file) => ({ path: file.filename })) } },
          },
        },
      };
    }

    case 'PullRequestId': {
      const repo = repoOf(str('owner'), str('name'));
      const number = num('number');
      if (repo === null) return { data: { repository: null } };
      const pull = number === null ? undefined : store.findPull(repo, number);
      if (pull === undefined) return { data: { repository: { pullRequest: null } } };
      return { data: { repository: { pullRequest: { id: store.pullGlobalId(repo, pull), number: pull.number } } } };
    }

    case 'PullRequestCreate': {
      const input = isRecord(variables.input) ? variables.input : {};
      const repositoryId = typeof input.repositoryId === 'string' ? input.repositoryId : null;
      const title = typeof input.title === 'string' ? input.title : null;
      const headRefName = typeof input.headRefName === 'string' ? input.headRefName : null;
      const baseRefName = typeof input.baseRefName === 'string' ? input.baseRefName : null;
      const body = typeof input.body === 'string' ? input.body : '';
      if (repositoryId === null || title === null || headRefName === null || baseRefName === null) {
        return { data: { createPullRequest: null }, ...errors('createPullRequest needs repositoryId, title, headRefName and baseRefName') };
      }
      const resolved = store.resolveGlobalId(repositoryId);
      if (resolved === null) return { data: { createPullRequest: null }, ...errors(`Could not resolve to a Repository with the id of '${repositoryId}'.`) };
      const repo = resolved.repo;
      const duplicate = repo.pulls.find((pull) => pull.state === 'open' && pull.headRef === headRefName && pull.baseRef === baseRefName);
      const pull = duplicate ?? store.createPull(repo, title, body, headRefName, baseRefName);
      if (pull === null) return { data: { createPullRequest: null }, ...errors('could not create the pull request') };
      return {
        data: {
          createPullRequest: {
            pullRequest: { number: pull.number, id: store.pullGlobalId(repo, pull), state: 'OPEN', headRefName: pull.headRef, baseRefName: pull.baseRef },
          },
        },
      };
    }

    case 'MergePullRequest': {
      const input = isRecord(variables.input) ? variables.input : {};
      const pullRequestId = typeof input.pullRequestId === 'string' ? input.pullRequestId : null;
      if (pullRequestId === null) return { data: { mergePullRequest: null }, ...errors('mergePullRequest needs pullRequestId') };
      const resolved = store.resolveGlobalId(pullRequestId);
      if (resolved === null || resolved.pull === undefined) {
        return { data: { mergePullRequest: null }, ...errors(`Could not resolve to a PullRequest with the id of '${pullRequestId}'.`) };
      }
      const pull = resolved.pull;
      if (pull.state !== 'open' || pull.merged) {
        return { data: { mergePullRequest: { pullRequest: { number: pull.number, merged: pull.merged, state: pullState(pull) } } } };
      }
      if (store.pullConflicts(resolved.repo, pull)) {
        return { data: { mergePullRequest: null }, ...errors('Merge conflict') };
      }
      const merged = store.mergePull(resolved.repo, pull);
      if (merged === null) return { data: { mergePullRequest: null }, ...errors('Merge conflict') };
      return { data: { mergePullRequest: { pullRequest: { number: pull.number, merged: true, state: 'MERGED' } } } };
    }

    case 'AddComment': {
      const input = isRecord(variables.input) ? variables.input : {};
      const subjectId = typeof input.subjectId === 'string' ? input.subjectId : null;
      const body = typeof input.body === 'string' ? input.body : null;
      if (subjectId === null || body === null) return { data: { addComment: null }, ...errors('addComment needs subjectId and body') };
      const resolved = store.resolveGlobalId(subjectId);
      if (resolved === null || resolved.issue === undefined) {
        return { data: { addComment: null }, ...errors(`Could not resolve to an Issue with the id of '${subjectId}'.`) };
      }
      const comment = { id: resolved.repo.nextCommentId++, body };
      resolved.issue.comments.push(comment);
      return {
        data: {
          addComment: { commentEdge: { node: { id: store.commentGlobalId(resolved.repo, resolved.issue, comment), body } } },
        },
      };
    }

    case 'NodeRepository': {
      const id = str('id');
      if (id === null) return { data: { node: null } };
      const resolved = store.resolveGlobalId(id);
      if (resolved === null || resolved.kind !== 'Repository') return { data: { node: null } };
      const repo = resolved.repo;
      return { data: { node: { id: store.repoGlobalId(repo), name: repo.name, nameWithOwner: `${repo.owner}/${repo.name}` } } };
    }

    case 'IssuesPage': {
      const repo = repoOf(str('owner'), str('name'));
      const first = num('first') ?? 10;
      const after = decodeCursor(typeof variables.after === 'string' ? variables.after : null);
      if (repo === null) return { data: { repository: null } };
      const ascending = [...repo.issues].sort((a, b) => a.number - b.number);
      const window = after === null ? ascending : ascending.filter((issue) => issue.number > after);
      const nodes = window.slice(0, first).map((issue) => ({ number: issue.number }));
      const hasNextPage = window.length > first;
      const endCursor = nodes.length > 0 ? encodeCursor(nodes[nodes.length - 1]?.number as number) : null;
      return { data: { repository: { issues: { nodes, pageInfo: { hasNextPage, endCursor } } } } };
    }

    default:
      return { ...errors(`Unknown operation ${JSON.stringify(name)}`) };
  }
}

function parseCommitTree(repo: Repo, commitOid: string): string | null {
  const obj = repo.objects.get(commitOid);
  if (obj === undefined || obj.type !== 'commit') return null;
  const text = obj.data.toString('utf8');
  const head = text.split('\n\n')[0] ?? '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) return line.slice(5).trim();
  }
  return null;
}

// ---------------------------------------------------------------- HTTP entry

export function handleGraphQL(req: IncomingMessage, res: ServerResponse, store: Store, body: Buffer): void {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.toString('utf8')) as unknown;
  } catch {
    return sendJson(res, 400, { message: 'malformed JSON body' });
  }
  if (Array.isArray(parsed)) {
    // Batched operations: one response per operation, in order.
    const out = parsed.map((entry) => {
      if (!isRecord(entry) || typeof entry.query !== 'string') return errors('each batched operation needs a query string');
      return execute(store, { query: entry.query, variables: isRecord(entry.variables) ? entry.variables : {} });
    });
    return sendJson(res, 200, out);
  }
  if (!isRecord(parsed) || typeof parsed.query !== 'string') {
    return sendJson(res, 400, { message: 'a GraphQL operation needs a query string' });
  }
  const result = execute(store, { query: parsed.query, variables: isRecord(parsed.variables) ? parsed.variables : {} });
  return sendJson(res, 200, result);
}
