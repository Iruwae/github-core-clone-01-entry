// Git plumbing for the forge: pkt-line framing, pack v2 read/write (plain objects out,
// deltas resolved on read), commit/tag/tree parsing and building, and the fetch object
// selection the suite's fetch checks pin down exactly (reachable(wants) − reachable(haves),
// with shallow/deepen boundaries).
//
// All object ids are SHA-1 over `<type> <size>\0` + bytes. Objects are stored verbatim;
// the server never re-serializes a pushed commit, tree or tag.

import { createHash } from 'node:crypto';
import { inflateSync, deflateSync } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';

export interface GitObject {
  readonly type: string; // 'commit' | 'tree' | 'blob' | 'tag'
  readonly data: Buffer;
}

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

/** The object id of `type` + `data` (the `<type> <size>\0` header is hashed in). */
export function objectId(type: string, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([header, data]));
}

// ---------------------------------------------------------------- pkt-lines

export type PktKind = 'data' | 'flush' | 'delim';
export interface Pkt {
  readonly kind: PktKind;
  readonly payload: Buffer;
}

/** One pkt-line framing `payload` (string is utf8). */
export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  if (len > 0xffff) throw new Error(`pkt-line of ${body.length} bytes exceeds the 65520-byte frame`);
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 0, 'ascii');
  return Buffer.concat([head, body]);
}

export const FLUSH_PKT = Buffer.from('0000', 'ascii');
export const DELIM_PKT = Buffer.from('0001', 'ascii');

/** Encode a list of pkt-lines / flush / delim markers into one buffer. */
export function encodePktLines(items: ReadonlyArray<string | Buffer | 'flush' | 'delim'>): Buffer {
  const parts: Buffer[] = [];
  for (const item of items) {
    if (item === 'flush') parts.push(FLUSH_PKT);
    else if (item === 'delim') parts.push(DELIM_PKT);
    else parts.push(pktLine(item));
  }
  return Buffer.concat(parts);
}

/** Decode a whole buffer of pkt-lines. Throws on a malformed frame. */
export function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let offset = 0;
  while (offset < buf.length) {
    if (offset + 4 > buf.length) throw new Error('pkt-line truncated at length prefix');
    const text = buf.subarray(offset, offset + 4).toString('ascii');
    const len = parseInt(text, 16);
    if (!Number.isFinite(len) || len < 0 || len > 0xffff) throw new Error(`bad pkt-line length ${JSON.stringify(text)}`);
    if (len === 0) {
      out.push({ kind: 'flush', payload: Buffer.alloc(0) });
      offset += 4;
    } else if (len === 1) {
      out.push({ kind: 'delim', payload: Buffer.alloc(0) });
      offset += 4;
    } else {
      if (len < 4) throw new Error(`pkt-line length ${len} is below the 4-byte minimum`);
      if (offset + len > buf.length) throw new Error('pkt-line truncated at payload');
      out.push({ kind: 'data', payload: Buffer.from(buf.subarray(offset + 4, offset + len)) });
      offset += len;
    }
  }
  return out;
}

// ---------------------------------------------------------------- deltas

/** Apply a git delta to `base`, per the delta instruction format. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      if (pos >= delta.length) throw new Error('delta truncated in varint');
      const byte = delta[pos] as number;
      pos += 1;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) return value;
    }
  };
  const baseSize = readVarint();
  if (baseSize !== base.length) throw new Error(`delta base size ${baseSize} does not match ${base.length}`);
  const resultSize = readVarint();
  const parts: Buffer[] = [];
  let produced = 0;
  while (pos < delta.length) {
    const op = delta[pos] as number;
    pos += 1;
    if (op & 0x80) {
      // Copy from base: offset/size bitfields.
      let offset = 0;
      let size = 0;
      for (let i = 0; i < 4; i += 1) {
        if (op & (1 << i)) {
          if (pos >= delta.length) throw new Error('delta truncated in copy offset');
          offset |= (delta[pos] as number) << (8 * i);
          pos += 1;
        }
      }
      for (let i = 0; i < 3; i += 1) {
        if (op & (0x10 << i)) {
          if (pos >= delta.length) throw new Error('delta truncated in copy size');
          size |= (delta[pos] as number) << (8 * i);
          pos += 1;
        }
      }
      if (size === 0) size = 0x10000;
      if (offset + size > base.length) throw new Error('delta copy runs past the base');
      parts.push(base.subarray(offset, offset + size));
      produced += size;
    } else if (op !== 0) {
      // Insert literal bytes.
      if (pos + op > delta.length) throw new Error('delta truncated in insert');
      parts.push(delta.subarray(pos, pos + op));
      pos += op;
      produced += op;
    } else {
      throw new Error('delta has a zero opcode');
    }
  }
  if (produced !== resultSize) throw new Error(`delta produced ${produced} bytes, expected ${resultSize}`);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- packs

const PACK_TYPES: Readonly<Record<number, string>> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag', 6: 'ofs-delta', 7: 'ref-delta' };
const TYPE_CODES: Readonly<Record<string, number>> = { commit: 1, tree: 2, blob: 3, tag: 4 };

/** Adler-32, the checksum a zlib stream ends with (deferred modulo, NMAX blocks). */
function adler32(data: Buffer): number {
  const MOD = 65521;
  let a = 1;
  let b = 0;
  let offset = 0;
  while (offset < data.length) {
    const end = Math.min(data.length, offset + 5552);
    for (; offset < end; offset += 1) {
      a += data[offset] as number;
      b += a;
    }
    a %= MOD;
    b %= MOD;
  }
  return (((b << 16) | a) >>> 0) as number;
}

/**
 * The byte length of the zlib stream starting at `offset`, whose inflated bytes are `data`.
 * The stream ends with adler32(data); scan for that trailer (verifying with one confirming
 * inflate) instead of binary-searching with trial inflates — a failed inflate throws, and
 * throwing in Node captures a stack, which dominated readPack's cost.
 */
function zlibStreamLength(buf: Buffer, offset: number, data: Buffer): number {
  const adler = adler32(data);
  for (let pos = offset + 2; pos + 4 <= buf.length; pos += 1) {
    if (buf.readUInt32BE(pos) !== adler) continue;
    try {
      const check = inflateSync(buf.subarray(offset, pos + 4));
      if (check.equals(data)) return pos + 4 - offset;
    } catch {
      // A coincidental adler value inside the deflate data: keep scanning.
    }
  }
  throw new Error('pack object has no valid zlib stream');
}

/** Inflate the zlib stream at `offset`, returning its bytes and the stream's byte length. */
function inflateAt(buf: Buffer, offset: number): { data: Buffer; length: number } {
  const data = inflateSync(buf.subarray(offset));
  return { data, length: zlibStreamLength(buf, offset, data) };
}

/**
 * Parse a pack v2 buffer into objects keyed by id. Verifies the trailer SHA-1.
 * Resolves OFS/REF deltas against pack entries (and, for REF deltas, against `resolveBase`
 * when the base is not in the pack — the client hands its held objects for thin packs).
 */
export function readPack(buf: Buffer, resolveBase?: (oid: string) => GitObject | undefined): Map<string, GitObject> {
  if (buf.length < 32) throw new Error('pack too short for a header and trailer');
  if (buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack does not start with PACK');
  const version = buf.readUInt32BE(4);
  if (version !== 2) throw new Error(`pack version ${version}, expected 2`);
  const count = buf.readUInt32BE(8);
  const trailer = buf.subarray(buf.length - 20);
  const expect = createHash('sha1').update(buf.subarray(0, buf.length - 20)).digest();
  if (!expect.equals(trailer)) throw new Error('pack trailer checksum mismatch');
  const objects = new Map<string, GitObject>();
  interface RawEntry {
    readonly offset: number;
    readonly typeCode: number;
    readonly data: Buffer;
    readonly baseOffset?: number;
    readonly baseOid?: string;
  }
  const raw: RawEntry[] = [];
  let offset = 12;
  for (let i = 0; i < count; i += 1) {
    const entryOffset = offset;
    if (offset >= buf.length - 20) throw new Error('pack truncated at an object header');
    let byte = buf[offset] as number;
    offset += 1;
    const typeCode = (byte >> 4) & 0x7;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      if (offset >= buf.length - 20) throw new Error('pack truncated in an object size');
      byte = buf[offset] as number;
      offset += 1;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let baseOffset: number | undefined;
    let baseOid: string | undefined;
    if (typeCode === 6) {
      // OFS_DELTA: negative offset varint.
      if (offset >= buf.length - 20) throw new Error('pack truncated in an ofs-delta offset');
      byte = buf[offset] as number;
      offset += 1;
      let value = byte & 0x7f;
      while (byte & 0x80) {
        if (offset >= buf.length - 20) throw new Error('pack truncated in an ofs-delta offset');
        byte = buf[offset] as number;
        offset += 1;
        value = ((value + 1) << 7) | (byte & 0x7f);
      }
      baseOffset = entryOffset - value;
      if (baseOffset < 0) throw new Error('ofs-delta names a base before the pack start');
    } else if (typeCode === 7) {
      if (offset + 20 > buf.length - 20) throw new Error('pack truncated in a ref-delta base id');
      baseOid = buf.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (PACK_TYPES[typeCode] === undefined) {
      throw new Error(`pack object has unknown type code ${typeCode}`);
    }
    const { data, length: streamLen } = inflateAt(buf, offset);
    if (data.length !== size) throw new Error(`pack object inflated to ${data.length} bytes, header says ${size}`);
    offset += streamLen;
    raw.push({ offset: entryOffset, typeCode, data, baseOffset, baseOid });
  }
  // Resolve deltas (entries may reference earlier entries only for OFS; REF may need the store).
  const byOffset = new Map<number, string>();
  const resolve = (entry: RawEntry, seen: Set<number>): GitObject => {
    if (entry.typeCode !== 6 && entry.typeCode !== 7) {
      const type = PACK_TYPES[entry.typeCode] as string;
      const oid = objectId(type, entry.data);
      byOffset.set(entry.offset, oid);
      return { type, data: entry.data };
    }
    if (seen.has(entry.offset)) throw new Error('pack delta cycle');
    seen.add(entry.offset);
    let base: GitObject | undefined;
    if (entry.typeCode === 6) {
      const baseEntry = raw.find((candidate) => candidate.offset === entry.baseOffset);
      if (baseEntry === undefined) throw new Error('ofs-delta base is not in the pack');
      base = resolve(baseEntry, seen);
    } else {
      const baseEntry = raw.find((candidate) => objectId(PACK_TYPES[candidate.typeCode] as string, candidate.data) === entry.baseOid);
      if (baseEntry !== undefined) base = resolve(baseEntry, seen);
      else if (resolveBase !== undefined) base = resolveBase(entry.baseOid as string);
      if (base === undefined) throw new Error(`ref-delta base ${entry.baseOid} is not available`);
    }
    const data = applyDelta(base.data, entry.data);
    const oid = objectId(base.type, data);
    byOffset.set(entry.offset, oid);
    return { type: base.type, data };
  };
  for (const entry of raw) {
    const obj = resolve(entry, new Set());
    objects.set(objectId(obj.type, obj.data), obj);
  }
  return objects;
}

function packObjectHeader(type: string, size: number): Buffer {
  const code = TYPE_CODES[type];
  if (code === undefined) throw new Error(`cannot pack type ${type}`);
  const bytes: number[] = [];
  let first = (code << 4) | (size & 0x0f);
  size >>= 4;
  if (size > 0) first |= 0x80;
  bytes.push(first);
  while (size > 0) {
    let byte = size & 0x7f;
    size >>= 7;
    if (size > 0) byte |= 0x80;
    bytes.push(byte);
  }
  return Buffer.from(bytes);
}

/** Write a pack v2 of plain objects (no deltas — the suite's pack-deltas check passes with 0). */
export function writePack(objects: ReadonlyArray<GitObject>): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    parts.push(packObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

/** A valid empty pack (12-byte header + 20-byte trailer), for fetches with nothing to send. */
export function emptyPack(): Buffer {
  return writePack([]);
}

// ---------------------------------------------------------------- commits, tags, trees

export interface CommitInfo {
  readonly tree: string;
  readonly parents: string[];
  readonly author: string;
  readonly committer: string;
  readonly message: string;
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const head = text.split('\n\n')[0] ?? '';
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  const message = text.includes('\n\n') ? text.slice(text.indexOf('\n\n') + 2) : '';
  return { tree, parents, author, committer, message };
}

export interface TagInfo {
  readonly object: string;
  readonly type: string;
  readonly tag: string;
  readonly tagger: string;
}

export function parseTag(data: Buffer): TagInfo {
  const text = data.toString('utf8');
  const head = text.split('\n\n')[0] ?? '';
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) tagger = line.slice(7);
  }
  return { object, type, tag, tagger };
}

export interface TreeEntry {
  readonly mode: string;
  readonly name: string;
  readonly oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) throw new Error('tree entry has no mode');
    const mode = data.subarray(offset, space).toString('ascii');
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) throw new Error('tree entry has no name or id');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

/** Git's tree sort: directories compare as if their name ended in '/'. */
export function treeSortKey(name: string, isDir: boolean): string {
  return isDir ? `${name}/` : name;
}

/** Serialize tree entries in git order (dirs sort as name + '/'). */
export function buildTree(entries: ReadonlyArray<TreeEntry>): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = treeSortKey(a.name, a.mode === '40000');
    const kb = treeSortKey(b.name, b.mode === '40000');
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

/** Build a commit object's bytes (verbatim layout, no re-serialization elsewhere). */
export function buildCommit(tree: string, parents: ReadonlyArray<string>, author: string, committer: string, message: string): Buffer {
  const lines: string[] = [`tree ${tree}`];
  for (const parent of parents) lines.push(`parent ${parent}`);
  lines.push(`author ${author}`, `committer ${committer}`, '', message);
  return Buffer.from(lines.join('\n'), 'utf8');
}

// ---------------------------------------------------------------- fetch selection

export interface FetchSelection {
  /** Objects to send (commits, trees, blobs, tags), in dependency-safe order. */
  readonly objects: GitObject[];
  /** New shallow boundaries (commits at the depth limit that still have parents). */
  readonly shallow: string[];
  /** Request shallow boundaries that are no longer shallow (a parent was included). */
  readonly unshallow: string[];
}

export type ObjectStore = ReadonlyMap<string, GitObject>;

/**
 * Select the objects a fetch sends, exactly as the suite's fetch checks require:
 * reachable(wants) − reachable(haves), with a depth limit and shallow boundaries.
 *
 * Commit walk (depth = parent steps from a want; a commit is visited while depth < limit):
 * - a have that is not a request shallow boundary stops the walk (its ancestors stay out);
 * - a have that IS a request shallow boundary is not sent, but the walk continues through it
 *   (deepening past it); a want that is also a have but not a boundary sends nothing at all.
 * Trees and blobs come from the sent commits' trees, minus anything reachable from a
 * have-commit's own tree (shared blobs are not re-sent). Tag wants send the tag plus its target.
 */
export function selectFetchObjects(
  store: ObjectStore,
  wants: ReadonlyArray<string>,
  haves: ReadonlyArray<string>,
  shallowBoundaries: ReadonlyArray<string>,
  depth: number | null,
): FetchSelection {
  const haveSet = new Set(haves);
  const shallowSet = new Set(shallowBoundaries);
  const limit = depth === null ? Number.POSITIVE_INFINITY : depth;
  const sentCommits = new Set<string>();
  const shallowOut: string[] = [];
  const unshallowOut: string[] = [];
  const visited = new Map<string, number>(); // commit oid -> shallowest depth visited

  // BFS over commits (queue of [oid, depth]); a commit reached at a shallower depth wins.
  const queue: Array<{ oid: string; depth: number }> = [];
  const tagTargets: string[] = [];
  const sentTags = new Set<string>();
  const enqueue = (oid: string, depth: number): void => {
    const prev = visited.get(oid);
    if (prev !== undefined && prev <= depth) return;
    visited.set(oid, depth);
    queue.push({ oid, depth });
  };
  for (const want of wants) {
    const obj = store.get(want);
    if (obj === undefined) continue; // a want the server does not hold: nothing to send for it
    if (obj.type === 'tag') {
      sentTags.add(want);
      const target = parseTag(obj.data).object;
      tagTargets.push(target);
      enqueue(target, 0);
    } else if (obj.type === 'commit') {
      enqueue(want, 0);
    } else {
      // A tree or blob wanted directly: send it as-is (no commit walk).
      // Handled below via directObjects.
    }
  }
  // Direct (non-commit, non-tag) wants.
  const directObjects = new Map<string, GitObject>();
  for (const want of wants) {
    const obj = store.get(want);
    if (obj !== undefined && obj.type !== 'commit' && obj.type !== 'tag' && !haveSet.has(want)) {
      directObjects.set(want, obj);
    }
  }

  while (queue.length > 0) {
    const current = queue.shift() as { oid: string; depth: number };
    if (visited.get(current.oid) !== current.depth) continue; // superseded by a shallower visit
    if (current.depth >= limit) continue;
    const obj = store.get(current.oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    const isHave = haveSet.has(current.oid);
    if (isHave && !shallowSet.has(current.oid)) {
      // Held and not a boundary: stop here, send nothing below.
      continue;
    }
    const commit = parseCommit(obj.data);
    if (!isHave) {
      sentCommits.add(current.oid);
      if (current.depth === limit - 1 && commit.parents.length > 0) shallowOut.push(current.oid);
    }
    // Walk parents (through boundaries and sent commits alike, within the limit).
    for (const parent of commit.parents) enqueue(parent, current.depth + 1);
  }

  // Unshallow: a request boundary whose parent was included is no longer shallow
  // (the history below it is now present, e.g. deepening past the old tip).
  for (const boundary of shallowBoundaries) {
    const obj = store.get(boundary);
    if (obj !== undefined && obj.type === 'commit') {
      const parents = parseCommit(obj.data).parents;
      if (parents.some((parent) => sentCommits.has(parent))) unshallowOut.push(boundary);
    }
  }

  // Trees/blobs: everything reachable from sent commits' trees, minus anything reachable
  // from a have-commit's tree. Tag targets that are commits are already in sentCommits
  // when visited; a tag target that is a tree/blob is handled as a direct object.
  const excludedTreeObjects = new Set<string>();
  const collectTreeObjects = (treeOid: string, into: Set<string>): void => {
    const stack = [treeOid];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (into.has(oid)) continue;
      const obj = store.get(oid);
      if (obj === undefined) continue;
      into.add(oid);
      if (obj.type === 'tree') {
        for (const entry of parseTree(obj.data)) stack.push(entry.oid);
      }
    }
  };
  for (const have of haves) {
    const obj = store.get(have);
    if (obj !== undefined && obj.type === 'commit') collectTreeObjects(parseCommit(obj.data).tree, excludedTreeObjects);
    else if (obj !== undefined && obj.type === 'tree') collectTreeObjects(have, excludedTreeObjects);
  }
  const send = new Map<string, GitObject>();
  for (const oid of sentTags) {
    const obj = store.get(oid);
    if (obj !== undefined) send.set(oid, obj);
  }
  for (const [oid, obj] of directObjects) send.set(oid, obj);
  for (const oid of sentCommits) {
    const obj = store.get(oid);
    if (obj !== undefined) send.set(oid, obj);
    if (obj !== undefined) {
      const treeObjects = new Set<string>();
      collectTreeObjects(parseCommit(obj.data).tree, treeObjects);
      for (const treeOid of treeObjects) {
        if (excludedTreeObjects.has(treeOid)) continue;
        const treeObj = store.get(treeOid);
        if (treeObj !== undefined) send.set(treeOid, treeObj);
      }
    }
  }
  // Tag targets that are trees/blobs (non-commit): send them too.
  for (const target of tagTargets) {
    const obj = store.get(target);
    if (obj !== undefined && obj.type !== 'commit' && !excludedTreeObjects.has(target)) send.set(target, obj);
  }

  // Dependency-safe order: blobs/trees before the commits that name them is not required
  // by the client (it indexes by id), but emit trees/blobs first anyway, then tags, commits.
  const rank = (type: string): number => (type === 'blob' ? 0 : type === 'tree' ? 1 : type === 'tag' ? 2 : 3);
  const objects = [...send.values()].sort((a, b) => rank(a.type) - rank(b.type));
  return { objects, shallow: shallowOut, unshallow: unshallowOut };
}

/** All object ids reachable from `tips` (commits, their trees/blobs, tag targets). */
export function reachableIds(store: ObjectStore, tips: ReadonlyArray<string>): Set<string> {
  const out = new Set<string>();
  const stack = [...tips];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (out.has(oid)) continue;
    const obj = store.get(oid);
    if (obj === undefined) continue;
    out.add(oid);
    if (obj.type === 'commit') {
      const commit = parseCommit(obj.data);
      stack.push(commit.tree, ...commit.parents);
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) stack.push(entry.oid);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    }
  }
  return out;
}
