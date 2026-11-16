// Git smart HTTP: info/refs advertisements (v2 for upload-pack when the client asks,
// v0 otherwise and always for receive-pack), v2 ls-refs / fetch, v0 fetch, and
// receive-pack pushes with per-ref validation and sideband report-status.
//
// Content types matter: advertisements are application/x-<service>-advertisement,
// POST results are application/x-<service>-result (the client's post() enforces them).

import type { IncomingMessage, ServerResponse } from 'node:http';
import { VIEWER } from './scenario.ts';
import {
  DELIM_PKT,
  FLUSH_PKT,
  ZERO_OID,
  decodePktLines,
  emptyPack,
  encodePktLines,
  parseTag,
  pktLine,
  readPack,
  selectFetchObjects,
  writePack,
  type GitObject,
} from './gitcore.ts';
import { Store, type Repo } from './store.ts';

const AGENT = 'github-forge/1.0.0';

function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length });
  res.end(body);
}

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

/** The peeled commit of an annotated tag ref (following tag objects), or null. */
function peeledCommit(repo: Repo, ref: string): string | null {
  let oid = repo.refs.get(ref) ?? null;
  if (oid === null) return null;
  for (let i = 0; i < 5 && oid !== null; i += 1) {
    const obj = repo.objects.get(oid);
    if (obj === undefined) return null;
    if (obj.type === 'commit') return oid;
    if (obj.type === 'tag') oid = parseTag(obj.data).object;
    else return null;
  }
  return null;
}

/** All refs sorted by name, each with its peeled commit when it is an annotated tag. */
function sortedRefs(repo: Repo): Array<{ name: string; oid: string; peeled: string | null }> {
  const names = [...repo.refs.keys()].sort();
  return names.map((name) => {
    const oid = repo.refs.get(name) as string;
    const obj = repo.objects.get(oid);
    const peeled = obj !== undefined && obj.type === 'tag' ? peeledCommit(repo, name) : null;
    return { name, oid, peeled };
  });
}

function v0Capabilities(service: string): string {
  const common = `symref=HEAD:refs/heads/main agent=${AGENT}`;
  if (service === 'git-receive-pack') {
    return `${common} report-status report-status-v2 side-band-64k delete-refs ofs-delta`;
  }
  return `${common} side-band-64k shallow ofs-delta`;
}

/** A v0 ref advertisement: service preamble, then HEAD + refs (+ peeled), then flush. */
function v0Advertisement(repo: Repo, service: string): Buffer {
  const refs = sortedRefs(repo);
  const caps = v0Capabilities(service);
  const lines: Array<string | Buffer | 'flush'> = [`# service=${service}\n`, 'flush'];
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    const mainTip = repo.refs.get('refs/heads/main') ?? refs[0]?.oid ?? ZERO_OID;
    lines.push(`${mainTip} HEAD\0${caps}\n`);
    for (const ref of refs) {
      lines.push(`${ref.oid} ${ref.name}\n`);
      if (ref.peeled !== null) lines.push(`${ref.peeled} ${ref.name}^{}\n`);
    }
  }
  lines.push('flush');
  return encodePktLines(lines);
}

/** A v2 capability advertisement for upload-pack: preamble, version 2, capabilities, flush. */
function v2Advertisement(): Buffer {
  return encodePktLines([
    '# service=git-upload-pack\n',
    'flush',
    'version 2\n',
    `agent=${AGENT}\n`,
    'ls-refs=unborn\n',
    'fetch=shallow wait-for-done\n',
    'object-format=sha1\n',
    'flush',
  ]);
}

/** GET /:owner/:repo.git/info/refs?service=... */
export function handleInfoRefs(req: IncomingMessage, res: ServerResponse, store: Store, owner: string, repoName: string): void {
  const url = new URL(req.url ?? '/', 'http://entry');
  const service = url.searchParams.get('service');
  const repo = store.getRepo(owner, repoName);
  if (repo === undefined || owner !== VIEWER.login) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    sendJson(res, 400, { message: 'unknown service' });
    return;
  }
  const contentType = `application/x-${service}-advertisement`;
  if (service === 'git-upload-pack' && (req.headers['git-protocol'] ?? '') === 'version=2') {
    sendBuffer(res, 200, contentType, v2Advertisement());
    return;
  }
  sendBuffer(res, 200, contentType, v0Advertisement(repo, service));
}

// ---------------------------------------------------------------- ls-refs (v2)

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

/** POST /:owner/:repo.git/git-upload-pack — ls-refs or fetch, both protocol v2. */
export function handleUploadPack(req: IncomingMessage, res: ServerResponse, store: Store, owner: string, repoName: string, body: Buffer): void {
  const repo = store.getRepo(owner, repoName);
  if (repo === undefined || owner !== VIEWER.login) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  const resultType = 'application/x-git-upload-pack-result';
  let packets;
  try {
    packets = decodePktLines(body);
  } catch {
    // A malformed body must not crash the server: answer an empty result and stay healthy.
    sendBuffer(res, 200, resultType, FLUSH_PKT);
    return;
  }
  const lines = packets.filter((packet) => packet.kind === 'data').map((packet) => packet.payload.toString('utf8'));
  const command = (lines[0] ?? '').trim();
  if (command === 'command=ls-refs') {
    const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
    for (const line of lines.slice(1)) {
      const text = line.trim();
      if (text === 'peel') args.peel = true;
      else if (text === 'symrefs') args.symrefs = true;
      else if (text === 'unborn') args.unborn = true;
      else if (text.startsWith('ref-prefix ')) args.prefixes.push(text.slice('ref-prefix '.length));
    }
    sendBuffer(res, 200, resultType, lsRefsResponse(repo, args));
    return;
  }
  if (command === 'command=fetch') {
    sendBuffer(res, 200, resultType, fetchV2Response(repo, lines.slice(1)));
    return;
  }
  // Not a v2 command: the client never sends v0 fetch here in this suite (it negotiates v2
  // from the advertisement), but answer an empty result rather than failing the exchange.
  sendBuffer(res, 200, resultType, FLUSH_PKT);
}

function lsRefsResponse(repo: Repo, args: LsRefsArgs): Buffer {
  const refs = sortedRefs(repo);
  const mainTip = repo.refs.get('refs/heads/main') ?? null;
  const includeHead = args.prefixes.length === 0 || args.prefixes.some((prefix) => 'HEAD'.startsWith(prefix));
  const keep = (name: string): boolean => args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  const out: Buffer[] = [];
  if (refs.length === 0) {
    // Unborn HEAD: the clone-empty check reads the symref target off this line.
    if (includeHead && args.unborn) {
      out.push(pktLine(args.symrefs ? 'unborn HEAD symref-target:refs/heads/main\n' : 'unborn HEAD\n'));
    }
  } else {
    if (includeHead && mainTip !== null) {
      out.push(pktLine(args.symrefs ? `${mainTip} HEAD symref-target:refs/heads/main\n` : `${mainTip} HEAD\n`));
    }
    for (const ref of refs) {
      if (!keep(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (args.peel && ref.peeled !== null) line += ` peeled:${ref.peeled}`;
      out.push(pktLine(`${line}\n`));
    }
  }
  out.push(FLUSH_PKT);
  return Buffer.concat(out);
}

// ---------------------------------------------------------------- fetch (v2)

function fetchV2Response(repo: Repo, argLines: string[]): Buffer {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | null = null;
  for (const raw of argLines) {
    const line = raw.trim();
    if (line.startsWith('want ')) wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = Number(line.slice(7).trim());
      if (Number.isSafeInteger(n) && n >= 0) depth = n === 0 ? null : n; // deepen 0 = unshallow all
    }
    // thin-pack / no-progress / ofs-delta / agent / object-format / done: no server action.
  }
  const selection = selectFetchObjects(repo.objects, wants, haves, shallow, depth);
  const parts: Buffer[] = [];
  // acknowledgments section.
  parts.push(pktLine('acknowledgments\n'));
  if (haves.length === 0) {
    parts.push(pktLine('NAK\n'));
  } else {
    const held = haves.filter((have) => repo.objects.has(have));
    for (const have of held) parts.push(pktLine(`ACK ${have}\n`));
    if (held.length === haves.length) parts.push(pktLine('ready\n'));
    else if (held.length === 0) parts.push(pktLine('NAK\n'));
  }
  parts.push(DELIM_PKT);
  // shallow-info section, only when there is something to say.
  if (selection.shallow.length > 0 || selection.unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of selection.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of selection.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(DELIM_PKT);
  }
  // packfile section: always at least one sideband packet (an empty pack when nothing is new).
  parts.push(pktLine('packfile\n'));
  const pack = selection.objects.length > 0 ? writePack(selection.objects) : emptyPack();
  const CHUNK = 32768;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    const chunk = pack.subarray(offset, Math.min(pack.length, offset + CHUNK));
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  parts.push(FLUSH_PKT);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- fetch (v0)

export function handleUploadPackV0(req: IncomingMessage, res: ServerResponse, store: Store, owner: string, repoName: string, body: Buffer): void {
  const repo = store.getRepo(owner, repoName);
  if (repo === undefined || owner !== VIEWER.login) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  const resultType = 'application/x-git-upload-pack-result';
  let packets;
  try {
    packets = decodePktLines(body);
  } catch {
    sendBuffer(res, 200, resultType, FLUSH_PKT);
    return;
  }
  // Two sections split by the first flush: wants, then haves/done.
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | null = null;
  let secondSection = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      secondSection = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').trim();
    if (secondSection) {
      if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    } else {
      if (line.startsWith('want ')) wants.push(line.slice(5).trim().split(' ')[0] as string);
      else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
      else if (line.startsWith('deepen ')) {
        const n = Number(line.slice(7).trim());
        if (Number.isSafeInteger(n) && n >= 0) depth = n === 0 ? null : n;
      }
    }
  }
  const selection = selectFetchObjects(repo.objects, wants, haves, shallow, depth);
  const parts: Buffer[] = [];
  if (haves.length === 0) {
    parts.push(pktLine('NAK\n'));
  } else {
    const held = haves.filter((have) => repo.objects.has(have));
    for (const have of held) parts.push(pktLine(`ACK ${have}\n`));
    if (held.length === 0) parts.push(pktLine('NAK\n'));
  }
  for (const oid of selection.shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of selection.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  const pack = selection.objects.length > 0 ? writePack(selection.objects) : emptyPack();
  const CHUNK = 32768;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    const chunk = pack.subarray(offset, Math.min(pack.length, offset + CHUNK));
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  parts.push(FLUSH_PKT);
  sendBuffer(res, 200, resultType, Buffer.concat(parts));
}

// ---------------------------------------------------------------- receive-pack

interface PushCommand {
  readonly oldOid: string;
  readonly newOid: string;
  readonly ref: string;
}

/** POST /:owner/:repo.git/git-receive-pack */
export function handleReceivePack(req: IncomingMessage, res: ServerResponse, store: Store, owner: string, repoName: string, body: Buffer): void {
  const repo = store.getRepo(owner, repoName);
  if (repo === undefined || owner !== VIEWER.login) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  const resultType = 'application/x-git-receive-pack-result';
  const report = (lines: string[], sideband: boolean): void => {
    // Each report line is pkt-framed inside band 1 (the client's parsePushReport decodes
    // the concatenated band-1 bytes as pkt-lines); without sideband, plain pkt-lines.
    const parts: Buffer[] = [];
    for (const line of lines) {
      const inner = pktLine(line);
      parts.push(sideband ? pktLine(Buffer.concat([Buffer.from([1]), inner])) : inner);
    }
    parts.push(FLUSH_PKT);
    sendBuffer(res, 200, resultType, Buffer.concat(parts));
  };

  let commands: PushCommand[] = [];
  let sideband = false;
  let packOffset = 0;
  try {
    // Decode pkt-lines only up to the flush that ends the command list; the raw pack
    // bytes after it are not pkt-lines and must not be parsed as such.
    const decoded = decodePktLinesUntilFlush(body);
    packOffset = decoded.offset;
    let first = true;
    for (const packet of decoded.packets) {
      if (packet.kind === 'flush') break;
      if (packet.kind !== 'data') continue;
      const text = packet.payload.toString('utf8');
      // Track where the pkt-line stream ends so the raw pack bytes after it can be read.
      const line = text.replace(/\n$/, '');
      const nul = line.indexOf('\0');
      const head = nul >= 0 ? line.slice(0, nul) : line;
      if (first && nul >= 0) {
        const caps = line.slice(nul + 1).split(' ');
        sideband = caps.includes('side-band-64k');
      }
      first = false;
      const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$/.exec(head);
      if (match === null) throw new Error(`bad push command ${JSON.stringify(head.slice(0, 60))}`);
      commands.push({ oldOid: match[1] as string, newOid: match[2] as string, ref: match[3] as string });
    }
  } catch {
    // Unparseable command list: answer unpack ok with no ref lines rather than crashing.
    report(['unpack ok\n'], sideband);
    return;
  }

  let unpackOk = true;
  let unpackDetail = 'ok';
  const packBytes = body.subarray(packOffset);
  const hasPack = packBytes.length > 0;
  if (hasPack) {
    try {
      const objects = readPack(packBytes);
      for (const [oid, obj] of objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, obj as GitObject);
      }
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message.replace(/\n/g, ' ') : 'unpack failed';
    }
  }

  const lines: string[] = [`unpack ${unpackOk ? 'ok' : unpackDetail}\n`];
  for (const command of commands) {
    if (!unpackOk) {
      lines.push(`ng ${command.ref} unpack failed\n`);
      continue;
    }
    const current = repo.refs.get(command.ref) ?? null;
    if (command.newOid === ZERO_OID) {
      // Delete.
      if (current === null) lines.push(`ng ${command.ref} no such ref\n`);
      else if (current !== command.oldOid) lines.push(`ng ${command.ref} non-fast-forward\n`);
      else {
        repo.refs.delete(command.ref);
        lines.push(`ok ${command.ref}\n`);
      }
    } else if (command.oldOid === ZERO_OID) {
      // Create.
      if (current !== null) {
        lines.push(`ng ${command.ref} already exists\n`);
      } else if (!repo.objects.has(command.newOid)) {
        lines.push(`ng ${command.ref} object not found\n`);
      } else {
        repo.refs.set(command.ref, command.newOid);
        lines.push(`ok ${command.ref}\n`);
      }
    } else {
      // Update: old must match the tip and new must descend from old.
      if (current === null) {
        lines.push(`ng ${command.ref} no such ref\n`);
      } else if (current !== command.oldOid) {
        lines.push(`ng ${command.ref} non-fast-forward\n`);
      } else if (!repo.objects.has(command.newOid)) {
        lines.push(`ng ${command.ref} object not found\n`);
      } else if (!store.isAncestor(repo, command.oldOid, command.newOid)) {
        lines.push(`ng ${command.ref} non-fast-forward\n`);
      } else {
        repo.refs.set(command.ref, command.newOid);
        lines.push(`ok ${command.ref}\n`);
      }
    }
  }
  report(lines, sideband);
}

/** Decode pkt-lines from the start of `body` up to and including the first flush packet,
 *  returning the packets and the byte offset just past that flush (where raw pack bytes begin). */
function decodePktLinesUntilFlush(body: Buffer): { packets: Array<{ kind: 'data' | 'flush' | 'delim'; payload: Buffer }>; offset: number } {
  const packets: Array<{ kind: 'data' | 'flush' | 'delim'; payload: Buffer }> = [];
  let offset = 0;
  while (offset + 4 <= body.length) {
    const len = parseInt(body.subarray(offset, offset + 4).toString('ascii'), 16);
    if (!Number.isFinite(len) || len < 0 || len > 0xffff) throw new Error('bad pkt-line length in push commands');
    if (len === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      return { packets, offset: offset + 4 };
    }
    if (len === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (len < 4 || offset + len > body.length) throw new Error('pkt-line truncated in push commands');
    packets.push({ kind: 'data', payload: Buffer.from(body.subarray(offset + 4, offset + len)) });
    offset += len;
  }
  throw new Error('push commands end without a flush');
}

