// The forge's in-memory store. One process, one trial: everything lives in Maps, nothing
// touches the disk (the trial container mounts the repo read-only; /tmp writes are pointless
// for state the suite reads back over HTTP anyway).
//
// Determinism rule: no wall clock, no randomness in anything a response carries. Commit
// authorship is the viewer with a fixed timestamp; created/merged timestamps are one fixed
// ISO string. A live trial and its replay therefore answer byte-for-byte the same.

import { VIEWER } from './scenario.ts';
import {
  ZERO_OID,
  buildCommit,
  buildTree,
  objectId,
  parseCommit,
  parseTag,
  parseTree,
  reachableIds,
  type GitObject,
  type TreeEntry,
} from './gitcore.ts';

/** Fixed commit identity for contents/merge commits the server authors itself. */
export const COMMIT_AUTHOR = `${VIEWER.name} <${VIEWER.email}> 1700000000 +0000`;
/** Fixed ISO timestamp for every created_at / merged_at a response carries. */
export const FIXED_ISO = '2026-11-14T06:30:00.000Z';

export interface Label {
  readonly name: string;
  readonly color: string;
  readonly description: string;
}

export interface Comment {
  readonly id: number;
  readonly body: string;
}

export interface Issue {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: Comment[];
}

export interface Pull {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  readonly headRef: string;
  readonly baseRef: string;
  headSha: string;
  baseSha: string;
  /** The base tip when the PR was opened (the merge-base for conflict checks). */
  readonly mergeBaseSha: string;
}

export interface Repo {
  readonly id: number;
  readonly name: string;
  readonly owner: string;
  description: string;
  isPrivate: boolean;
  readonly defaultBranch: string;
  readonly objects: Map<string, GitObject>;
  readonly refs: Map<string, string>; // full ref -> oid
  readonly labels: Map<string, Label>;
  readonly issues: Issue[];
  readonly pulls: Pull[];
  nextNumber: number; // shared issue+PR counter, starts at 1
  nextCommentId: number;
  nextLabelId: number;
  createdSeq: number;
}

export class Store {
  readonly repos = new Map<string, Repo>(); // `${owner}/${name}` -> Repo
  private nextRepoId = 5000;
  private nextObjectId = 1; // stable numeric ids for issues/PRs/comments
  private repoSeq = 0;

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo {
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      owner,
      description,
      isPrivate,
      defaultBranch: 'main',
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      nextNumber: 1,
      nextCommentId: 1,
      nextLabelId: 1,
      createdSeq: this.repoSeq++,
    };
    this.repos.set(this.key(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  /** All of the viewer's repos, in creation order. */
  listRepos(owner: string): Repo[] {
    return [...this.repos.values()].filter((repo) => repo.owner === owner).sort((a, b) => a.createdSeq - b.createdSeq);
  }

  nextId(): number {
    return this.nextObjectId++;
  }

  // ------------------------------------------------------------ git objects

  putObject(repo: Repo, type: string, data: Buffer): string {
    const oid = objectId(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  /** The tip of a ref, or null. `ref` may be a full ref or a branch name. */
  resolveRef(repo: Repo, ref: string): string | null {
    if (repo.refs.has(ref)) return repo.refs.get(ref) as string;
    const full = ref.startsWith('refs/') ? ref : `refs/heads/${ref}`;
    return repo.refs.get(full) ?? null;
  }

  /** All refs under refs/heads/, as [branch, oid] sorted by branch name. */
  branches(repo: Repo): Array<{ name: string; sha: string }> {
    const out: Array<{ name: string; sha: string }> = [];
    for (const [ref, oid] of repo.refs) {
      if (ref.startsWith('refs/heads/')) out.push({ name: ref.slice('refs/heads/'.length), sha: oid });
    }
    out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    return out;
  }

  /** Is `ancestor` reachable from `oid` (commit ancestry, following all parents)? */
  isAncestor(repo: Repo, ancestor: string, oid: string): boolean {
    if (ancestor === oid) return true;
    const seen = new Set<string>();
    const stack = [oid];
    while (stack.length > 0) {
      const current = stack.pop() as string;
      if (current === ancestor) return true;
      if (seen.has(current)) continue;
      seen.add(current);
      const obj = repo.objects.get(current);
      if (obj !== undefined && obj.type === 'commit') stack.push(...parseCommit(obj.data).parents);
    }
    return false;
  }

  // ------------------------------------------------------------ trees

  /** The flat path -> {mode, oid} map of a tree, recursively. */
  flattenTree(repo: Repo, treeOid: string | null): Map<string, { mode: string; oid: string }> {
    const out = new Map<string, { mode: string; oid: string }>();
    if (treeOid === null) return out;
    const walk = (oid: string, prefix: string): void => {
      const obj = repo.objects.get(oid);
      if (obj === undefined || obj.type !== 'tree') return;
      for (const entry of parseTree(obj.data)) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        if (entry.mode === '40000') walk(entry.oid, path);
        else out.set(path, { mode: entry.mode, oid: entry.oid });
      }
    };
    walk(treeOid, '');
    return out;
  }

  /** Rebuild the tree for `root` with `path` set to `blobOid` (or removed when null). */
  setPathInTree(repo: Repo, rootTreeOid: string | null, path: string, blobOid: string | null): string {
    const parts = path.split('/').filter((part) => part !== '');
    if (parts.length === 0) throw new Error('empty file path');
    const build = (treeOid: string | null, depth: number): string => {
      const entries = new Map<string, TreeEntry>();
      if (treeOid !== null) {
        const obj = repo.objects.get(treeOid);
        if (obj !== undefined && obj.type === 'tree') {
          for (const entry of parseTree(obj.data)) entries.set(entry.name, entry);
        }
      }
      const part = parts[depth] as string;
      if (depth === parts.length - 1) {
        if (blobOid === null) entries.delete(part);
        else entries.set(part, { mode: '100644', name: part, oid: blobOid });
      } else {
        const child = entries.get(part);
        const childTree = child !== undefined && child.mode === '40000' ? child.oid : null;
        const newChild = build(childTree, depth + 1);
        const childObj = repo.objects.get(newChild);
        const empty = childObj !== undefined && childObj.type === 'tree' && parseTree(childObj.data).length === 0;
        if (empty) entries.delete(part);
        else entries.set(part, { mode: '40000', name: part, oid: newChild });
      }
      const data = buildTree([...entries.values()]);
      return this.putObject(repo, 'tree', data);
    };
    return build(rootTreeOid, 0);
  }

  // ------------------------------------------------------------ contents commits

  /**
   * Commit `path` -> `content` on `branch` (creating the branch from the default tip, or
   * from nothing when the repo is unborn). Returns the new commit oid.
   */
  commitFile(repo: Repo, branch: string, path: string, content: Buffer, message: string): string {
    const ref = `refs/heads/${branch}`;
    let tip = repo.refs.get(ref) ?? null;
    if (tip === null && branch !== repo.defaultBranch) {
      tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
    }
    const blobOid = this.putObject(repo, 'blob', content);
    const baseTree = tip === null ? null : parseCommit((repo.objects.get(tip) as GitObject).data).tree;
    const newTree = this.setPathInTree(repo, baseTree, path, blobOid);
    const parents = tip === null ? [] : [tip];
    const commitData = buildCommit(newTree, parents, COMMIT_AUTHOR, COMMIT_AUTHOR, message);
    const commitOid = this.putObject(repo, 'commit', commitData);
    repo.refs.set(ref, commitOid);
    return commitOid;
  }

  /** Delete `path` on `branch`. Returns the new commit oid, or null when the file is absent. */
  deleteFile(repo: Repo, branch: string, path: string, message: string): string | null {
    const ref = `refs/heads/${branch}`;
    const tip = repo.refs.get(ref) ?? null;
    if (tip === null) return null;
    const baseTree = parseCommit((repo.objects.get(tip) as GitObject).data).tree;
    const flat = this.flattenTree(repo, baseTree);
    if (!flat.has(path)) return null;
    const newTree = this.setPathInTree(repo, baseTree, path, null);
    const commitData = buildCommit(newTree, [tip], COMMIT_AUTHOR, COMMIT_AUTHOR, message);
    const commitOid = this.putObject(repo, 'commit', commitData);
    repo.refs.set(ref, commitOid);
    return commitOid;
  }

  /** Read a file or directory at `ref` (branch name or oid). */
  readPath(repo: Repo, ref: string, path: string): { kind: 'file'; blobOid: string; data: Buffer } | { kind: 'dir'; entries: TreeEntry[] } | null {
    let tip = this.resolveRef(repo, ref);
    if (tip === null && repo.objects.has(ref)) tip = ref;
    if (tip === null) return null;
    const obj = repo.objects.get(tip);
    if (obj === undefined) return null;
    const treeOid = obj.type === 'commit' ? parseCommit(obj.data).tree : obj.type === 'tree' ? tip : null;
    if (treeOid === null) return null;
    if (path === '' || path === '/') {
      const root = repo.objects.get(treeOid);
      return root !== undefined ? { kind: 'dir', entries: parseTree(root.data) } : null;
    }
    const flat = this.flattenTree(repo, treeOid);
    const file = flat.get(path);
    if (file !== undefined) {
      const blob = repo.objects.get(file.oid);
      if (blob === undefined) return null;
      return { kind: 'file', blobOid: file.oid, data: blob.data };
    }
    // A directory: walk to its tree.
    const parts = path.split('/').filter((part) => part !== '');
    let current = treeOid;
    for (const part of parts) {
      const objNow = repo.objects.get(current);
      if (objNow === undefined || objNow.type !== 'tree') return null;
      const entry = parseTree(objNow.data).find((candidate) => candidate.name === part && candidate.mode === '40000');
      if (entry === undefined) return null;
      current = entry.oid;
    }
    const dir = repo.objects.get(current);
    return dir !== undefined ? { kind: 'dir', entries: parseTree(dir.data) } : null;
  }

  /** Commits reachable from `ref`, newest-first (DFS pre-order from the tip). */
  listCommits(repo: Repo, ref: string): Array<{ sha: string; commit: ReturnType<typeof parseCommit> }> {
    let tip = this.resolveRef(repo, ref);
    if (tip === null && repo.objects.has(ref)) tip = ref;
    if (tip === null) return [];
    const out: Array<{ sha: string; commit: ReturnType<typeof parseCommit> }> = [];
    const seen = new Set<string>();
    const stack = [tip];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.objects.get(oid);
      if (obj === undefined || obj.type !== 'commit') continue;
      const commit = parseCommit(obj.data);
      out.push({ sha: oid, commit });
      // Push parents reversed so the first parent is visited first.
      for (let i = commit.parents.length - 1; i >= 0; i -= 1) stack.push(commit.parents[i] as string);
    }
    return out;
  }

  // ------------------------------------------------------------ issues & pulls

  findIssue(repo: Repo, number: number): Issue | undefined {
    return repo.issues.find((issue) => issue.number === number);
  }

  findPull(repo: Repo, number: number): Pull | undefined {
    return repo.pulls.find((pull) => pull.number === number);
  }

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue {
    const issue: Issue = {
      id: this.nextId(),
      number: repo.nextNumber++,
      title,
      body,
      state: 'open',
      labels: [...labels],
      comments: [],
    };
    repo.issues.push(issue);
    return issue;
  }

  createPull(repo: Repo, title: string, body: string, headRef: string, baseRef: string): Pull | null {
    const headSha = this.resolveRef(repo, headRef);
    const baseSha = this.resolveRef(repo, baseRef);
    if (headSha === null || baseSha === null) return null;
    const pull: Pull = {
      id: this.nextId(),
      number: repo.nextNumber++,
      title,
      body,
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef,
      baseRef,
      headSha,
      baseSha,
      mergeBaseSha: baseSha,
    };
    repo.pulls.push(pull);
    return pull;
  }

  // ------------------------------------------------------------ pull diffs & merges

  /** Line-diff counts between two blobs (simple LCS on lines). */
  static lineDiff(a: Buffer, b: Buffer): { additions: number; deletions: number } {
    const linesA = Store.splitLines(a);
    const linesB = Store.splitLines(b);
    const m = linesA.length;
    const n = linesB.length;
    // LCS lengths, one row at a time (files here are small).
    let prev = new Array<number>(n + 1).fill(0);
    for (let i = 1; i <= m; i += 1) {
      const cur = new Array<number>(n + 1).fill(0);
      for (let j = 1; j <= n; j += 1) {
        cur[j] = linesA[i - 1] === linesB[j - 1] ? (prev[j - 1] as number) + 1 : Math.max(prev[j] as number, cur[j - 1] as number);
      }
      prev = cur;
    }
    const lcs = prev[n] as number;
    return { additions: n - lcs, deletions: m - lcs };
  }

  private static splitLines(data: Buffer): string[] {
    const text = data.toString('utf8');
    if (text === '') return [];
    const lines = text.split('\n');
    if (lines[lines.length - 1] === '') lines.pop();
    return lines;
  }

  /** The file-level diff between two trees (base tip vs head tip), recursive. */
  diffTrees(repo: Repo, baseTree: string | null, headTree: string | null): Array<{
    filename: string;
    status: 'added' | 'removed' | 'modified';
    additions: number;
    deletions: number;
  }> {
    const base = this.flattenTree(repo, baseTree);
    const head = this.flattenTree(repo, headTree);
    const names = new Set([...base.keys(), ...head.keys()]);
    const out: Array<{ filename: string; status: 'added' | 'removed' | 'modified'; additions: number; deletions: number }> = [];
    for (const name of [...names].sort()) {
      const b = base.get(name);
      const h = head.get(name);
      if (b !== undefined && h === undefined) {
        const data = repo.objects.get(b.oid)?.data ?? Buffer.alloc(0);
        const lines = Store.splitLines(data).length;
        out.push({ filename: name, status: 'removed', additions: 0, deletions: lines });
      } else if (b === undefined && h !== undefined) {
        const data = repo.objects.get(h.oid)?.data ?? Buffer.alloc(0);
        const lines = Store.splitLines(data).length;
        out.push({ filename: name, status: 'added', additions: lines, deletions: 0 });
      } else if (b !== undefined && h !== undefined && b.oid !== h.oid) {
        const dataB = repo.objects.get(b.oid)?.data ?? Buffer.alloc(0);
        const dataH = repo.objects.get(h.oid)?.data ?? Buffer.alloc(0);
        const diff = Store.lineDiff(dataB, dataH);
        out.push({ filename: name, status: 'modified', additions: diff.additions, deletions: diff.deletions });
      }
    }
    return out;
  }

  /** Files changed between two commits' trees (flat path -> blob oid). */
  private treeDiffMap(repo: Repo, fromTree: string | null, toTree: string | null): Map<string, string | null> {
    const from = this.flattenTree(repo, fromTree);
    const to = this.flattenTree(repo, toTree);
    const out = new Map<string, string | null>();
    const names = new Set([...from.keys(), ...to.keys()]);
    for (const name of names) {
      const a = from.get(name)?.oid ?? null;
      const b = to.get(name)?.oid ?? null;
      if (a !== b) out.set(name, b);
    }
    return out;
  }

  /** Would merging this PR conflict? A file changed on both sides of the merge-base with
   *  different resulting content is a conflict (the suite's fixture shape). */
  pullConflicts(repo: Repo, pull: Pull): boolean {
    const baseTip = this.resolveRef(repo, pull.baseRef);
    const headTip = this.resolveRef(repo, pull.headRef);
    if (baseTip === null || headTip === null) return false;
    if (baseTip === pull.mergeBaseSha) return false; // base has not moved: no conflict possible
    const baseCommit = repo.objects.get(baseTip);
    const headCommit = repo.objects.get(headTip);
    const mergeBaseCommit = repo.objects.get(pull.mergeBaseSha);
    if (baseCommit === undefined || headCommit === undefined || mergeBaseCommit === undefined) return false;
    const headChanges = this.treeDiffMap(repo, parseCommit(mergeBaseCommit.data).tree, parseCommit(headCommit.data).tree);
    const baseChanges = this.treeDiffMap(repo, parseCommit(mergeBaseCommit.data).tree, parseCommit(baseCommit.data).tree);
    for (const [path, headOid] of headChanges) {
      if (baseChanges.has(path) && baseChanges.get(path) !== headOid) return true;
    }
    return false;
  }

  /** Squash-merge a PR onto its base branch. Returns the merge commit oid, or null on conflict. */
  mergePull(repo: Repo, pull: Pull): string | null {
    if (pull.state !== 'open' || pull.merged) return null;
    if (this.pullConflicts(repo, pull)) return null;
    const baseTip = this.resolveRef(repo, pull.baseRef);
    const headTip = this.resolveRef(repo, pull.headRef);
    if (baseTip === null || headTip === null) return null;
    const baseCommit = repo.objects.get(baseTip) as GitObject | undefined;
    const headCommit = repo.objects.get(headTip) as GitObject | undefined;
    const mergeBaseCommit = repo.objects.get(pull.mergeBaseSha) as GitObject | undefined;
    if (baseCommit === undefined || headCommit === undefined || mergeBaseCommit === undefined) return null;
    // New tree: base tip's tree with the head's changes (vs merge-base) applied.
    const baseTree = parseCommit(baseCommit.data).tree;
    const headChanges = this.treeDiffMap(repo, parseCommit(mergeBaseCommit.data).tree, parseCommit(headCommit.data).tree);
    let newTree = baseTree;
    for (const [path, oid] of headChanges) {
      newTree = this.setPathInTree(repo, newTree, path, oid);
    }
    const message = `Merge pull request #${pull.number} from ${pull.headRef}`;
    const commitData = buildCommit(newTree, [baseTip], COMMIT_AUTHOR, COMMIT_AUTHOR, message);
    const mergeOid = this.putObject(repo, 'commit', commitData);
    repo.refs.set(`refs/heads/${pull.baseRef}`, mergeOid);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = FIXED_ISO;
    pull.mergeCommitSha = mergeOid;
    pull.baseSha = mergeOid;
    return mergeOid;
  }

  // ------------------------------------------------------------ global ids (GraphQL Relay)

  globalId(kind: string, key: string): string {
    return Buffer.from(`${kind}:${key}`, 'utf8').toString('base64');
  }

  repoGlobalId(repo: Repo): string {
    return this.globalId('Repository', `${repo.owner}/${repo.name}`);
  }

  issueGlobalId(repo: Repo, issue: Issue): string {
    return this.globalId('Issue', `${repo.owner}/${repo.name}/${issue.number}`);
  }

  pullGlobalId(repo: Repo, pull: Pull): string {
    return this.globalId('PullRequest', `${repo.owner}/${repo.name}/${pull.number}`);
  }

  commentGlobalId(repo: Repo, issue: Issue, comment: Comment): string {
    return this.globalId('IssueComment', `${repo.owner}/${repo.name}/${issue.number}/${comment.id}`);
  }

  /** Resolve a global id back to a repo / issue / pull / comment. */
  resolveGlobalId(id: string): { kind: string; repo: Repo; issue?: Issue; pull?: Pull; comment?: Comment } | null {
    let decoded: string;
    try {
      decoded = Buffer.from(id, 'base64').toString('utf8');
    } catch {
      return null;
    }
    const [kind, rest] = decoded.split(':', 2) as [string, string];
    if (kind === undefined || rest === undefined) return null;
    if (kind === 'Repository') {
      const [owner, name] = rest.split('/', 2) as [string, string];
      const repo = this.getRepo(owner, name);
      return repo !== undefined ? { kind, repo } : null;
    }
    const parts = rest.split('/');
    if (parts.length < 3) return null;
    const owner = parts[0] as string;
    const name = parts[1] as string;
    const number = Number(parts[2]);
    const repo = this.getRepo(owner, name);
    if (repo === undefined || !Number.isSafeInteger(number)) return null;
    if (kind === 'Issue') {
      const issue = this.findIssue(repo, number);
      if (issue === undefined) return null;
      if (parts.length === 4) {
        const comment = issue.comments.find((candidate) => candidate.id === Number(parts[3]));
        return comment !== undefined ? { kind: 'IssueComment', repo, issue, comment } : null;
      }
      return { kind, repo, issue };
    }
    if (kind === 'PullRequest') {
      const pull = this.findPull(repo, number);
      return pull !== undefined ? { kind, repo, pull } : null;
    }
    return null;
  }
}

export { ZERO_OID, reachableIds };
