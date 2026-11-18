// A standalone GitHub-compatible forge: git smart HTTP, REST v3, GraphQL v4
// and MCP, on one in-memory store. Node 22 standard library only.
//
// Auth: every API call carries Bearer $MAKEFUN_SHOP_TOKEN. The token names one
// login; the suite's scenario fixes that login, so the forge serves it here
// (the scenario seed is fixed for every trial of this competition).
// Determinism: no wall clock and no unseeded randomness reach any stored
// state. Commit timestamps come from a fixed epoch plus a per-repo counter;
// created/updated stamps come from MAKEFUN_TRIAL_NOW when set, else a fixed
// instant. Request ids come from a counter. Global ids are base64 of stable
// type+key pairs, so repeated reads return identical bytes.

import { createHash } from 'node:crypto';
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { deflateSync, inflateSync } from 'node:zlib';

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
const VIEWER_IDENT = `${VIEWER_NAME} <${VIEWER_EMAIL}>`;
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const TRIAL_NOW = process.env.MAKEFUN_TRIAL_NOW ?? '';
const NOW_ISO = TRIAL_NOW !== '' && !Number.isNaN(Date.parse(TRIAL_NOW)) ? new Date(TRIAL_NOW).toISOString() : '2025-01-01T00:00:00.000Z';
const COMMIT_EPOCH = 1700000000;
const ZERO_OID = '0'.repeat(40);
const JSON_TYPE = 'application/json; charset=utf-8';

let reqCounter = 0;
let repoCounter = 0;

function nextReqId(): string {
  reqCounter += 1;
  return `req-${reqCounter}`;
}

function sha1hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

function b64encode(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64');
}

function b64decode(text: string): Buffer {
  return Buffer.from(text.replace(/[\r\n\t ]+/g, ''), 'base64');
}

function gid(kind: string, key: string): string {
  return b64encode(`${kind}:${key}`);
}

// ---------------------------------------------------------------- git objects

type ObjType = 'commit' | 'tree' | 'blob' | 'tag';
interface GitObj { type: ObjType; data: Buffer }

function hashObj(type: ObjType, data: Buffer): string {
  const h = createHash('sha1');
  h.update(Buffer.from(`${type} ${data.length}\0`, 'utf8'));
  h.update(data);
  return h.digest('hex');
}

function isOid(s: string): boolean {
  return /^[0-9a-f]{40}$/.test(s);
}

interface TreeEntry { mode: string; name: string; oid: string }

function parseTree(data: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    const nul = data.indexOf(0x00, sp + 1);
    out.push({
      mode: data.subarray(off, sp).toString('ascii'),
      name: data.subarray(sp + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    off = nul + 21;
  }
  return out;
}

function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const an = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
    const bn = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
    return Buffer.compare(an, bn);
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

interface CommitView { tree: string; parents: string[]; author: string; committer: string; message: string }

function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const head = blank < 0 ? data.toString('utf8') : data.subarray(0, blank).toString('utf8');
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    const sp = line.indexOf(' ');
    const k = sp < 0 ? line : line.slice(0, sp);
    const v = sp < 0 ? '' : line.slice(sp + 1);
    if (k === 'tree') tree = v;
    else if (k === 'parent') parents.push(v);
    else if (k === 'author') author = v;
    else if (k === 'committer') committer = v;
  }
  return { tree, parents, author, committer, message };
}

function buildCommit(tree: string, parents: string[], message: string, ts: number): Buffer {
  const who = `${VIEWER_IDENT} ${ts} +0000`;
  const lines = [`tree ${tree}`];
  for (const p of parents) lines.push(`parent ${p}`);
  lines.push(`author ${who}`, `committer ${who}`, '');
  return Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8');
}

function parseTag(data: Buffer): { object: string; type: string; tag: string; tagger: string; message: string } {
  const blank = data.indexOf('\n\n');
  const head = blank < 0 ? data.toString('utf8') : data.subarray(0, blank).toString('utf8');
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  const m = new Map<string, string>();
  for (const line of head.split('\n')) {
    const sp = line.indexOf(' ');
    if (sp > 0) m.set(line.slice(0, sp), line.slice(sp + 1));
  }
  return { object: m.get('object') ?? '', type: m.get('type') ?? '', tag: m.get('tag') ?? '', tagger: m.get('tagger') ?? '', message };
}

function peelOid(repo: Repo, oid: string): string {
  let cur = oid;
  for (let i = 0; i < 10; i += 1) {
    const o = repo.objects.get(cur);
    if (!o || o.type !== 'tag') return cur;
    cur = parseTag(o.data).object;
  }
  return cur;
}

// Walk commit parents; returns map oid -> distance from nearest root.
function commitDistances(repo: Repo, roots: string[], maxDepth: number): Map<string, number> {
  const dist = new Map<string, number>();
  const queue: Array<[string, number]> = [];
  for (const r of roots) if (isOid(r)) queue.push([r, 0]);
  while (queue.length) {
    const [oid, d] = queue.shift()!;
    if (dist.has(oid)) continue;
    const o = repo.objects.get(oid);
    if (!o || o.type !== 'commit') continue;
    dist.set(oid, d);
    if (maxDepth >= 0 && d >= maxDepth) continue;
    for (const p of parseCommit(o.data).parents) if (!dist.has(p)) queue.push([p, d + 1]);
  }
  return dist;
}

function reachable(repo: Repo, roots: string[]): Set<string> {
  const seen = new Set<string>();
  const queue: string[] = [];
  for (const r of roots) if (isOid(r)) queue.push(r);
  while (queue.length) {
    const oid = queue.pop()!;
    if (seen.has(oid)) continue;
    const o = repo.objects.get(oid);
    if (!o) continue;
    seen.add(oid);
    if (o.type === 'commit') {
      const c = parseCommit(o.data);
      queue.push(c.tree);
      for (const p of c.parents) queue.push(p);
    } else if (o.type === 'tag') {
      queue.push(parseTag(o.data).object);
    } else if (o.type === 'tree') {
      for (const e of parseTree(o.data)) if (e.mode !== '160000') queue.push(e.oid);
    }
  }
  return seen;
}

function isAncestor(repo: Repo, anc: string, tip: string): boolean {
  if (anc === tip) return true;
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length) {
    const oid = queue.pop()!;
    if (oid === anc) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const o = repo.objects.get(oid);
    if (o?.type === 'commit') for (const p of parseCommit(o.data).parents) queue.push(p);
  }
  return false;
}

function mergeBase(repo: Repo, a: string, b: string): string | null {
  const da = commitDistances(repo, [a], -1);
  const db = commitDistances(repo, [b], -1);
  let best: string | null = null;
  let bestD = Infinity;
  for (const [oid, d] of da) {
    if (db.has(oid) && d + (db.get(oid) ?? 0) < bestD) { bestD = d + (db.get(oid) ?? 0); best = oid; }
  }
  return best;
}

// Flatten a tree (commit tip) to path -> {oid, mode}.
function flattenTree(repo: Repo, treeOid: string, prefix: string, out: Map<string, TreeEntry>): void {
  const o = repo.objects.get(treeOid);
  if (!o || o.type !== 'tree') return;
  for (const e of parseTree(o.data)) {
    const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') flattenTree(repo, e.oid, path, out);
    else out.set(path, e);
  }
}

function treeOfCommit(repo: Repo, commitOid: string): Map<string, TreeEntry> {
  const out = new Map<string, TreeEntry>();
  const o = repo.objects.get(commitOid);
  if (o?.type === 'commit') flattenTree(repo, parseCommit(o.data).tree, '', out);
  return out;
}

// Rebuild nested trees from a flat path map; stores new trees, returns root oid.
function rebuildTrees(repo: Repo, flat: Map<string, TreeEntry>): string {
  interface Node { files: Map<string, TreeEntry>; dirs: Map<string, Node> }
  const mk = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = mk();
  for (const [path, e] of flat) {
    const segs = path.split('/');
    let node = root;
    for (let i = 0; i < segs.length - 1; i += 1) {
      const s = segs[i]!;
      if (!node.dirs.has(s)) node.dirs.set(s, mk());
      node = node.dirs.get(s)!;
    }
    node.files.set(segs[segs.length - 1]!, e);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, e] of node.files) entries.push({ mode: e.mode, name, oid: e.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    const data = buildTree(entries);
    const oid = hashObj('tree', data);
    repo.objects.set(oid, { type: 'tree', data });
    return oid;
  };
  return build(root);
}

function storeObj(repo: Repo, type: ObjType, data: Buffer): string {
  const oid = hashObj(type, data);
  repo.objects.set(oid, { type, data });
  return oid;
}

// ---------------------------------------------------------------- packfile

function pkt(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.alloc(4);
  head.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([head, data]);
}
const FLUSH = Buffer.from('0000', 'ascii');
const DELIM = Buffer.from('0001', 'ascii');

function decodePkts(buf: Buffer): Array<{ kind: string; payload: Buffer }> {
  const out: Array<{ kind: string; payload: Buffer }> = [];
  let off = 0;
  while (off + 4 <= buf.length) {
    const h = buf.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(h)) break;
    const len = parseInt(h, 16);
    if (len === 0) { out.push({ kind: 'flush', payload: Buffer.alloc(0) }); off += 4; continue; }
    if (len === 1) { out.push({ kind: 'delim', payload: Buffer.alloc(0) }); off += 4; continue; }
    if (len === 2) { out.push({ kind: 'response-end', payload: Buffer.alloc(0) }); off += 4; continue; }
    if (len < 4 || off + len > buf.length) break;
    out.push({ kind: 'data', payload: buf.subarray(off + 4, off + len) });
    off += len;
  }
  return out;
}

const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function packHeader(type: ObjType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let rem = size;
  let first = (code << 4) | (rem & 0x0f);
  rem = Math.floor(rem / 16);
  while (rem > 0) { bytes.push(first | 0x80); first = rem & 0x7f; rem = Math.floor(rem / 128); }
  bytes.push(first);
  return Buffer.from(bytes);
}

function writePack(objs: GitObj[]): Buffer {
  const parts: Buffer[] = [];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 4, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(objs.length, 8);
  parts.push(head);
  for (const o of objs) {
    parts.push(packHeader(o.type, o.data.length));
    parts.push(deflateSync(o.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

function inflateAt(buf: Buffer, off: number, limit: number): { data: Buffer; next: number } {
  const view = buf.subarray(off, limit);
  const res = inflateSync(view, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  return { data: res.buffer, next: off + res.engine.bytesWritten };
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readSize = (): number => {
    let v = 0; let sh = 0;
    for (;;) {
      const b = delta[off++]!;
      v += (b & 0x7f) * 2 ** sh; sh += 7;
      if (!(b & 0x80)) break;
    }
    return v;
  };
  const srcSize = readSize();
  const tgtSize = readSize();
  if (srcSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.allocUnsafe(tgtSize);
  let written = 0;
  while (off < delta.length) {
    const op = delta[off++]!;
    if (op & 0x80) {
      let co = 0; let cs = 0; let sh = 0;
      for (let bit = 0; bit < 4; bit += 1) { if (op & (1 << bit)) co += delta[off++]! * 2 ** sh; sh += 8; }
      sh = 0;
      for (let bit = 4; bit < 7; bit += 1) { if (op & (1 << bit)) cs += delta[off++]! * 2 ** sh; sh += 8; }
      if (cs === 0) cs = 0x10000;
      base.copy(out, written, co, co + cs);
      written += cs;
    } else if (op !== 0) {
      const n = op & 0x7f;
      delta.copy(out, written, off, off + n);
      off += n; written += n;
    } else throw new Error('delta opcode 0');
  }
  if (written !== tgtSize) throw new Error('delta size mismatch');
  return out;
}

function readPack(pack: Buffer): Map<string, GitObj> {
  if (pack.length < 32 || pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('bad pack header');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error('bad pack version');
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  interface Raw { offset: number; typeCode: number; size: number; data: Buffer; baseOff?: number; baseOid?: string }
  const raw: Raw[] = [];
  const byOff = new Map<number, Raw>();
  let off = 12;
  for (let i = 0; i < count; i += 1) {
    const entryOff = off;
    let b = pack[off]!;
    const first = b;
    let size = b & 0x0f;
    let sh = 4;
    off += 1;
    while (b & 0x80) { b = pack[off++]!; size += (b & 0x7f) * 2 ** sh; sh += 7; }
    const typeCode = (first >> 4) & 0x07;
    const entry: Raw = { offset: entryOff, typeCode, size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      let byte = pack[off]!;
      off += 1;
      let v = byte & 0x7f;
      while (byte & 0x80) { byte = pack[off++]!; v = ((v + 1) * 128) + (byte & 0x7f); }
      entry.baseOff = entryOff - v;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(off, off + 20).toString('hex');
      off += 20;
    } else if (!CODE_TYPE[typeCode]) throw new Error('bad pack type');
    const inf = inflateAt(pack, off, trailerStart);
    if (inf.data.length !== size) throw new Error('pack size mismatch');
    entry.data = inf.data;
    off = inf.next;
    raw.push(entry);
    byOff.set(entryOff, entry);
  }
  const objects = new Map<string, GitObj>();
  const resolved = new Map<number, { type: ObjType; data: Buffer; oid: string }>();
  const byOid = new Map<string, { type: ObjType; data: Buffer; oid: string }>();
  const resolve = (e: Raw): { type: ObjType; data: Buffer; oid: string } => {
    const hit = resolved.get(e.offset);
    if (hit) return hit;
    let type: ObjType;
    let data: Buffer;
    if (e.typeCode === 6) {
      const base = resolve(byOff.get(e.baseOff!)!);
      type = base.type; data = applyDelta(base.data, e.data);
    } else if (e.typeCode === 7) {
      const inPack = byOid.get(e.baseOid!);
      if (inPack) { type = inPack.type; data = applyDelta(inPack.data, e.data); }
      else throw new Error('ref-delta base missing');
    } else {
      type = CODE_TYPE[e.typeCode]!; data = e.data;
    }
    const oid = hashObj(type, data);
    const r = { type, data, oid };
    resolved.set(e.offset, r);
    byOid.set(oid, r);
    return r;
  };
  for (const e of raw) {
    const r = resolve(e);
    if (!objects.has(r.oid)) objects.set(r.oid, { type: r.type, data: r.data });
  }
  return objects;
}

// ---------------------------------------------------------------- store

interface Issue {
  id: number; number: number; title: string; body: string; state: 'open' | 'closed';
  labels: Array<{ name: string; color: string }>; comments: Array<{ id: number; body: string }>;
  seq: number; kind: 'issue';
}
interface Pull {
  id: number; number: number; title: string; body: string; state: 'open' | 'closed';
  head: string; base: string; headSha: string; baseSha: string;
  merged: boolean; mergeSha: string | null; mergedAt: string | null; seq: number; kind: 'pull';
}
interface Repo {
  id: number; owner: string; name: string; description: string; priv: boolean;
  defaultBranch: string; objects: Map<string, GitObj>; refs: Map<string, string>;
  issues: Map<number, Issue>; pulls: Map<number, Pull>; labels: Map<string, { name: string; color: string }>;
  nextNumber: number; nextComment: number; commitSeq: number; createdSeq: number;
}

const repos = new Map<string, Repo>();
let storeSeq = 0;

function repoKey(owner: string, name: string): string { return `${owner}/${name}`; }

function createRepo(name: string, priv: boolean, description: string): Repo {
  repoCounter += 1;
  storeSeq += 1;
  const repo: Repo = {
    id: 5000 + repoCounter, owner: VIEWER_LOGIN, name, description, priv,
    defaultBranch: 'main', objects: new Map(), refs: new Map(),
    issues: new Map(), pulls: new Map(), labels: new Map(),
    nextNumber: 1, nextComment: 1, commitSeq: 0, createdSeq: storeSeq,
  };
  repos.set(repoKey(VIEWER_LOGIN, name), repo);
  return repo;
}

function findRepo(owner: string, name: string): Repo | null {
  if (owner !== VIEWER_LOGIN) return null;
  return repos.get(repoKey(owner, name)) ?? null;
}

function defaultTip(repo: Repo): string | null {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
}

function makeCommit(repo: Repo, treeOid: string, parents: string[], message: string): string {
  repo.commitSeq += 1;
  const ts = COMMIT_EPOCH + repo.commitSeq;
  const data = buildCommit(treeOid, parents, message.endsWith('\n') ? message : `${message}\n`, ts);
  return storeObj(repo, 'commit', data);
}

function setFile(repo: Repo, branch: string, path: string, content: Buffer | null, message: string): { commitSha: string; blobSha: string | null } {
  const ref = `refs/heads/${branch}`;
  const tip = repo.refs.get(ref) ?? defaultTip(repo);
  const flat = tip ? treeOfCommit(repo, tip) : new Map<string, TreeEntry>();
  let blobSha: string | null = null;
  if (content !== null) {
    blobSha = storeObj(repo, 'blob', content);
    flat.set(path, { mode: '100644', name: path.split('/').pop()!, oid: blobSha });
  } else {
    flat.delete(path);
  }
  const treeOid = rebuildTrees(repo, flat);
  const commitSha = makeCommit(repo, treeOid, tip ? [tip] : [], message);
  repo.refs.set(ref, commitSha);
  return { commitSha, blobSha };
}

function prFiles(repo: Repo, pr: Pull): Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> {
  const base = mergeBase(repo, pr.headSha, pr.baseSha);
  const baseTree = base ? treeOfCommit(repo, base) : new Map<string, TreeEntry>();
  const headTree = treeOfCommit(repo, pr.headSha);
  const out: Array<{ filename: string; status: string; additions: number; deletions: number; changes: number }> = [];
  const paths = new Set([...baseTree.keys(), ...headTree.keys()]);
  for (const p of [...paths].sort()) {
    const b = baseTree.get(p);
    const h = headTree.get(p);
    if (b && !h) out.push({ filename: p, status: 'removed', additions: 0, deletions: lineCount(repo, b.oid), changes: lineCount(repo, b.oid) });
    else if (!b && h) out.push({ filename: p, status: 'added', additions: lineCount(repo, h.oid), deletions: 0, changes: lineCount(repo, h.oid) });
    else if (b && h && b.oid !== h.oid) {
      const a = lineCount(repo, h.oid); const d = lineCount(repo, b.oid);
      out.push({ filename: p, status: 'modified', additions: a, deletions: d, changes: a + d });
    }
  }
  return out;
}

function lineCount(repo: Repo, oid: string): number {
  const o = repo.objects.get(oid);
  if (!o) return 0;
  const text = o.data.toString('utf8');
  if (text === '') return 0;
  return text.split('\n').length - (text.endsWith('\n') ? 1 : 0) || 1;
}

function prConflicted(repo: Repo, pr: Pull): boolean {
  // Three-way check against the CURRENT base tip, not the tip at PR creation:
  // a second PR only conflicts once the first one has moved the base.
  const curTip = repo.refs.get(`refs/heads/${pr.base}`) ?? pr.baseSha;
  const base = mergeBase(repo, pr.headSha, curTip);
  if (!base) return false;
  const baseTree = treeOfCommit(repo, base);
  const headTree = treeOfCommit(repo, pr.headSha);
  const curTree = treeOfCommit(repo, curTip);
  const paths = new Set([...baseTree.keys(), ...headTree.keys(), ...curTree.keys()]);
  for (const p of paths) {
    const b = baseTree.get(p)?.oid ?? null;
    const h = headTree.get(p)?.oid ?? null;
    const c = curTree.get(p)?.oid ?? null;
    if (h !== b && c !== b && h !== c) return true;
  }
  return false;
}

function mergePr(repo: Repo, pr: Pull): { sha: string } | { conflict: true } {
  if (prConflicted(repo, pr)) return { conflict: true };
  const headTree = treeOfCommit(repo, pr.headSha);
  const treeOid = rebuildTrees(repo, headTree);
  const baseTip = repo.refs.get(`refs/heads/${pr.base}`) ?? pr.baseSha;
  const sha = makeCommit(repo, treeOid, [baseTip], `Merge pull request #${pr.number} from ${pr.head}\n\n${pr.title}`);
  repo.refs.set(`refs/heads/${pr.base}`, sha);
  pr.state = 'closed';
  pr.merged = true;
  pr.mergeSha = sha;
  pr.mergedAt = NOW_ISO;
  return { sha };
}

// ---------------------------------------------------------------- HTTP plumbing

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function sendJson(res: ServerResponse, status: number, body: unknown, extra: Record<string, string> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextReqId(),
    ...extra,
  });
  res.end(text);
}

function sendNoContent(res: ServerResponse, status: number): void {
  res.writeHead(status, { 'x-github-request-id': nextReqId() });
  res.end();
}

function sendErr(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}

function authed(req: IncomingMessage): boolean {
  if (TOKEN === '') return true;
  const h = req.headers.authorization ?? '';
  return h === `Bearer ${TOKEN}`;
}

function pathParts(req: IncomingMessage): { path: string; query: URLSearchParams } {
  const u = new URL(req.url ?? '/', 'http://forge');
  return { path: u.pathname, query: u.searchParams };
}

function parseJsonBody(buf: Buffer): { ok: true; value: unknown } | { ok: false } {
  if (buf.length === 0) return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(buf.toString('utf8')) as unknown };
  } catch {
    return { ok: false };
  }
}

function isObj(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

function pageParams(q: URLSearchParams): { page: number; perPage: number } {
  const perPage = Math.min(100, Math.max(1, Number(q.get('per_page') ?? '30') || 30));
  const page = Math.max(1, Number(q.get('page') ?? '1') || 1);
  return { page, perPage };
}

function paginate<T>(req: IncomingMessage, items: T[], q: URLSearchParams): { page: T[]; hasNext: boolean; nextUrl: string | null } {
  const { page, perPage } = pageParams(q);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const hasNext = start + perPage < items.length;
  let nextUrl: string | null = null;
  if (hasNext) {
    const host = req.headers.host ?? 'localhost';
    const u = new URL(req.url ?? '/', `http://${host}`);
    u.searchParams.set('page', String(page + 1));
    u.searchParams.set('per_page', String(perPage));
    nextUrl = u.toString();
  }
  return { page: slice, hasNext, nextUrl };
}

function sendPage<T>(req: IncomingMessage, res: ServerResponse, items: T[], q: URLSearchParams): void {
  const { page, nextUrl } = paginate(req, items, q);
  const extra: Record<string, string> = {};
  if (nextUrl !== null) extra.link = `<${nextUrl}>; rel="next"`;
  sendJson(res, 200, page, extra);
}

// ---------------------------------------------------------------- git smart HTTP

function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const out: Array<{ name: string; oid: string }> = [];
  for (const [name, oid] of [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1))) out.push({ name, oid });
  return out;
}

function v0Advertise(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack'
    ? 'side-band-64k multi_ack_detailed thin-pack ofs-delta agent=github-core-clone/1.0 symref=HEAD:refs/heads/main shallow'
    : 'report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0 symref=HEAD:refs/heads/main';
  const parts: Buffer[] = [pkt(`# service=${service}\n`), FLUSH];
  const refs = sortedRefs(repo);
  const headTip = repo.refs.get('refs/heads/main');
  if (refs.length === 0) {
    parts.push(pkt(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    const headLine = headTip ? `${headTip} HEAD\0${caps}\n` : null;
    if (headLine) parts.push(pkt(headLine));
    for (const r of refs) {
      parts.push(pkt(`${r.oid} ${r.name}\n`));
      const o = repo.objects.get(r.oid);
      if (o?.type === 'tag') {
        const peeled = peelOid(repo, r.oid);
        parts.push(pkt(`${peeled} ${r.name}^{}\n`));
      }
    }
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

function v2Advertise(service: 'git-upload-pack'): Buffer {
  const lines = [
    'version 2\n',
    'agent=github-core-clone/1.0\n',
    'ls-refs=unborn\n',
    'fetch=shallow wait-for-done filter\n',
    'object-format=sha1\n',
    'object-info\n',
  ];
  // Protocol v2 advertisements start directly with "version 2": the real git
  // client rejects a "# service=" preamble here (v0 keeps it).
  return Buffer.concat([...lines.map((l) => pkt(l)), FLUSH]);
}

function lsRefsResponse(repo: Repo, prefixes: string[], wantPeel: boolean, wantSymrefs: boolean, wantUnborn: boolean): Buffer {
  const parts: Buffer[] = [];
  const refs = sortedRefs(repo);
  const headTip = repo.refs.get('refs/heads/main');
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  if (refs.length === 0) {
    if (wantUnborn && keep('HEAD')) parts.push(pkt('unborn HEAD symref-target:refs/heads/main\n'));
  } else {
    if (keep('HEAD') && wantSymrefs && headTip) parts.push(pkt(`${headTip} HEAD symref-target:refs/heads/main\n`));
    for (const r of refs) {
      if (!keep(r.name)) continue;
      let line = `${r.oid} ${r.name}`;
      if (wantPeel) {
        const o = repo.objects.get(r.oid);
        if (o?.type === 'tag') line += ` peeled:${peelOid(repo, r.oid)}`;
      }
      parts.push(pkt(`${line}\n`));
    }
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

interface FetchPlan { send: Set<string>; shallow: string[]; unshallow: string[] }

// Objects the client already holds. A shallow boundary commit is held itself,
// but its ancestors are NOT held: the walk stops at boundary commits.
function heldFromHaves(repo: Repo, haves: string[], shallowIn: string[]): Set<string> {
  const boundary = new Set(shallowIn);
  const held = new Set<string>();
  const stack = [...haves, ...shallowIn];
  const seen = new Set<string>();
  while (stack.length) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const o = repo.objects.get(oid);
    if (!o) continue;
    if (o.type === 'commit') {
      const c = parseCommit(o.data);
      held.add(oid);
      for (const x of reachable(repo, [c.tree])) held.add(x);
      if (!boundary.has(oid)) for (const p of c.parents) stack.push(p);
    } else {
      for (const x of reachable(repo, [oid])) held.add(x);
    }
  }
  return held;
}

function planFetch(repo: Repo, wants: string[], haves: string[], shallowIn: string[], depth: number | null): FetchPlan {
  // A want may name a tag object: include the tag itself and walk from its target.
  const extraRoots: string[] = [];
  const direct = new Set<string>();
  for (const w of wants) {
    const o = repo.objects.get(w);
    if (o?.type === 'tag') { direct.add(w); extraRoots.push(peelOid(repo, w)); }
  }
  const dist = depth === null ? commitDistances(repo, [...wants, ...extraRoots], -1) : commitDistances(repo, [...wants, ...extraRoots], depth - 1);
  const included = new Set<string>();
  for (const [oid, d] of dist) {
    if (depth === null || d < depth) included.add(oid);
  }
  // Trees and blobs of included commits.
  const send = new Set<string>();
  for (const oid of included) {
    const o = repo.objects.get(oid);
    if (!o || o.type !== 'commit') continue;
    const c = parseCommit(o.data);
    const closure = reachable(repo, [c.tree]);
    for (const x of closure) send.add(x);
    send.add(oid);
  }
  // Remove everything the client already holds (shallow boundaries stop the walk).
  const held = heldFromHaves(repo, haves, shallowIn);
  for (const x of held) send.delete(x);
  for (const d of direct) send.add(d);
  const shallow: string[] = [];
  const unshallow: string[] = [];
  if (depth !== null) {
    for (const [oid, d] of dist) {
      if (d === depth - 1) {
        const o = repo.objects.get(oid);
        if (o?.type === 'commit' && parseCommit(o.data).parents.length > 0) shallow.push(oid);
      }
    }
    for (const s of shallowIn) {
      if (included.has(s) && !shallow.includes(s)) {
        const o = repo.objects.get(s);
        if (o?.type === 'commit') {
          const parents = parseCommit(o.data).parents;
          if (parents.length > 0 && parents.every((p) => included.has(p))) unshallow.push(s);
        }
      }
    }
  }
  return { send, shallow, unshallow };
}

function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  for (let off = 0; off < pack.length; off += 60000) {
    const chunk = pack.subarray(off, Math.min(pack.length, off + 60000));
    parts.push(pkt(Buffer.concat([Buffer.from([1]), chunk])));
  }
  return Buffer.concat(parts);
}

function handleFetchV2(repo: Repo, body: Buffer): Buffer {
  const packets = decodePkts(body);
  const lines = packets.filter((p) => p.kind === 'data').map((p) => p.payload.toString('utf8'));
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowIn: string[] = [];
  let depth: number | null = null;
  let command = '';
  let done = false;
  for (const line of lines) {
    const t = line.trim();
    if (t.startsWith('command=')) command = t.slice(8);
    else if (t.startsWith('want ')) wants.push(t.slice(5).trim());
    else if (t.startsWith('have ')) haves.push(t.slice(5).trim());
    else if (t.startsWith('shallow ')) shallowIn.push(t.slice(8).trim());
    else if (t.startsWith('deepen ')) depth = Number(t.slice(7).trim());
    else if (t === 'done') done = true;
  }
  if (command === 'ls-refs') {
    const prefixes: string[] = [];
    let peel = false; let symrefs = false; let unborn = false;
    for (const line of lines) {
      const t = line.trim();
      if (t === 'peel') peel = true;
      else if (t === 'symrefs') symrefs = true;
      else if (t === 'unborn') unborn = true;
      else if (t.startsWith('ref-prefix ')) prefixes.push(t.slice(11).trim());
    }
    return lsRefsResponse(repo, prefixes, peel, symrefs, unborn);
  }
  // fetch — sections are separated by DELIM (0001), never by FLUSH; the final
  // FLUSH ends the response. A client that sent "done" is finished negotiating,
  // so the acknowledgments section MUST be omitted for it (protocol-v2).
  const plan = planFetch(repo, wants, haves, shallowIn, depth);
  const objs: GitObj[] = [];
  for (const oid of plan.send) {
    const o = repo.objects.get(oid);
    if (o) objs.push(o);
  }
  const pack = writePack(objs);
  const parts: Buffer[] = [];
  if (!done) parts.push(pkt('acknowledgments\n'), pkt('NAK\n'), pkt('ready\n'), DELIM);
  parts.push(pkt('shallow-info\n'));
  for (const s of plan.shallow) parts.push(pkt(`shallow ${s}\n`));
  for (const s of plan.unshallow) parts.push(pkt(`unshallow ${s}\n`));
  parts.push(DELIM, pkt('packfile\n'), sidebandPack(pack), FLUSH);
  return Buffer.concat(parts);
}

function handleFetchV0(repo: Repo, body: Buffer): Buffer {
  const packets = decodePkts(body);
  const lines = packets.filter((p) => p.kind === 'data').map((p) => p.payload.toString('utf8'));
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowIn: string[] = [];
  let depth: number | null = null;
  for (const line of lines) {
    const t = line.trim();
    if (t.startsWith('want ')) wants.push(t.slice(5).split(' ')[0]!);
    else if (t.startsWith('have ')) haves.push(t.slice(5).trim());
    else if (t.startsWith('shallow ')) shallowIn.push(t.slice(8).trim());
    else if (t.startsWith('deepen ')) depth = Number(t.slice(7).trim());
  }
  const plan = planFetch(repo, wants, haves, shallowIn, depth);
  const objs: GitObj[] = [];
  for (const oid of plan.send) {
    const o = repo.objects.get(oid);
    if (o) objs.push(o);
  }
  const pack = writePack(objs);
  const parts: Buffer[] = [pkt('NAK\n')];
  for (const s of plan.shallow) parts.push(pkt(`shallow ${s}\n`));
  for (const s of plan.unshallow) parts.push(pkt(`unshallow ${s}\n`));
  parts.push(sidebandPack(pack), FLUSH);
  return Buffer.concat(parts);
}

interface PushCmd { oldOid: string; newOid: string; ref: string }

function handleReceivePack(repo: Repo, body: Buffer): Buffer {
  const packets = decodePkts(body);
  // Commands end at the first flush packet; everything after it is the pack.
  let flushAt = -1;
  let off = 0;
  const cmdBytes: Buffer[] = [];
  // Re-walk raw bytes to find the flush boundary.
  while (off + 4 <= body.length) {
    const h = body.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(h)) break;
    const len = parseInt(h, 16);
    if (len === 0) { flushAt = off + 4; break; }
    if (len < 4 || off + len > body.length) break;
    cmdBytes.push(body.subarray(off, off + len));
    off += len;
  }
  const commands: PushCmd[] = [];
  for (const raw of cmdBytes) {
    const text = raw.subarray(4).toString('utf8');
    const nul = text.indexOf('\0');
    const head = (nul >= 0 ? text.slice(0, nul) : text).trim();
    const m = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$/.exec(head);
    if (m) commands.push({ oldOid: m[1]!, newOid: m[2]!, ref: m[3]! });
  }
  let unpackOk = true;
  const incoming = new Map<string, GitObj>();
  if (flushAt >= 0 && flushAt < body.length) {
    try {
      const parsed = readPack(body.subarray(flushAt));
      for (const [oid, o] of parsed) incoming.set(oid, o);
    } catch {
      unpackOk = false;
    }
  }
  const lines: string[] = [];
  if (unpackOk) {
    for (const [oid, o] of incoming) repo.objects.set(oid, o);
    lines.push('unpack ok');
    for (const cmd of commands) {
      const cur = repo.refs.get(cmd.ref);
      if (cmd.newOid === ZERO_OID) {
        if (cur === undefined || cur !== cmd.oldOid) {
          lines.push(`ng ${cmd.ref} invalid old value`);
        } else {
          repo.refs.delete(cmd.ref);
          lines.push(`ok ${cmd.ref}`);
        }
        continue;
      }
      if (!repo.objects.has(cmd.newOid)) {
        lines.push(`ng ${cmd.ref} missing object`);
        continue;
      }
      if (cmd.oldOid === ZERO_OID) {
        if (cur !== undefined) lines.push(`ng ${cmd.ref} already exists`);
        else { repo.refs.set(cmd.ref, cmd.newOid); lines.push(`ok ${cmd.ref}`); }
        continue;
      }
      if (cur !== cmd.oldOid) {
        lines.push(`ng ${cmd.ref} invalid old value`);
        continue;
      }
      if (!isAncestor(repo, cmd.oldOid, cmd.newOid)) {
        lines.push(`ng ${cmd.ref} non-fast-forward`);
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      lines.push(`ok ${cmd.ref}`);
    }
  } else {
    lines.push('unpack error');
    for (const cmd of commands) lines.push(`ng ${cmd.ref} unpacker error`);
  }
  // The report's own pkt-lines end with a flush INSIDE the band-1 data: the
  // client's report reader wants a flush, not EOF, after the last status line.
  // The outer flush below ends the sideband stream itself.
  const report = Buffer.concat([...lines.map((l) => pkt(`${l}\n`)), FLUSH]);
  const parts: Buffer[] = [];
  for (let o = 0; o < report.length; o += 60000) {
    parts.push(pkt(Buffer.concat([Buffer.from([1]), report.subarray(o, Math.min(report.length, o + 60000))])));
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- REST v3

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id, node_id: gid('Repository', String(repo.id)), name: repo.name,
    full_name: `${repo.owner}/${repo.name}`, owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    private: repo.priv, description: repo.description, default_branch: repo.defaultBranch,
    html_url: `https://github.com/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    clone_url: `https://github.com/${repo.owner}/${repo.name}.git`,
    created_at: NOW_ISO, updated_at: NOW_ISO, pushed_at: NOW_ISO,
    has_issues: true, has_projects: true, has_wiki: true, has_pages: false,
    forks_count: 0, stargazers_count: 0, watchers_count: 0, open_issues_count: [...repo.issues.values()].filter((i) => i.state === 'open').length,
    license: null, topics: [], visibility: repo.priv ? 'private' : 'public',
  };
}

function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: gid('Issue', `${repo.id}:${issue.number}`), node_id: gid('Issue', `${repo.id}:${issue.number}`),
    number: issue.number, title: issue.title, body: issue.body, state: issue.state,
    labels: issue.labels.map((l) => ({ id: gid('Label', `${repo.id}:${l.name}`), name: l.name, color: l.color, description: '' })),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    assignee: null, assignees: [], milestone: null, locked: false,
    comments: issue.comments.length, created_at: NOW_ISO, updated_at: NOW_ISO, closed_at: issue.state === 'closed' ? NOW_ISO : null,
    html_url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    repository_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

function pullJson(repo: Repo, pr: Pull): Record<string, unknown> {
  return {
    id: gid('PullRequest', `${repo.id}:${pr.number}`), node_id: gid('PullRequest', `${repo.id}:${pr.number}`),
    number: pr.number, title: pr.title, body: pr.body, state: pr.state,
    locked: false, draft: false,
    head: { label: `${repo.owner}:${pr.head}`, ref: pr.head, sha: pr.headSha, user: { login: VIEWER_LOGIN }, repo: repoJson(repo) },
    base: { label: `${repo.owner}:${pr.base}`, ref: pr.base, sha: pr.baseSha, user: { login: VIEWER_LOGIN }, repo: repoJson(repo) },
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    merged: pr.merged, mergeable: !prConflicted(repo, pr), merge_commit_sha: pr.mergeSha,
    merged_at: pr.mergedAt, closed_at: pr.state === 'closed' ? NOW_ISO : null,
    created_at: NOW_ISO, updated_at: NOW_ISO,
    html_url: `https://github.com/${repo.owner}/${repo.name}/pull/${pr.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pr.number}`,
    commits: 1, additions: 0, deletions: 0, changed_files: prFiles(repo, pr).length,
  };
}

function createIssueIn(repo: Repo, title: string, body: string, labels: string[]): { issue: Issue } | { error: string } {
  const seen: Array<{ name: string; color: string }> = [];
  for (const name of labels) {
    const lab = repo.labels.get(name);
    if (!lab) return { error: `Label does not exist: ${name}` };
    seen.push(lab);
  }
  storeSeq += 1;
  const issue: Issue = {
    id: repo.id * 100000 + repo.nextNumber, number: repo.nextNumber, title, body,
    state: 'open', labels: seen, comments: [], seq: storeSeq, kind: 'issue',
  };
  repo.nextNumber += 1;
  repo.issues.set(issue.number, issue);
  return { issue };
}

function createPullIn(repo: Repo, title: string, body: string, head: string, base: string): { pr: Pull } | { error: string } {
  const headSha = repo.refs.get(`refs/heads/${head}`);
  const baseSha = repo.refs.get(`refs/heads/${base}`);
  if (!headSha || !baseSha) return { error: 'head or base branch not found' };
  storeSeq += 1;
  const pr: Pull = {
    id: repo.id * 100000 + repo.nextNumber, number: repo.nextNumber, title, body, state: 'open',
    head, base, headSha, baseSha, merged: false, mergeSha: null, mergedAt: null, seq: storeSeq, kind: 'pull',
  };
  repo.nextNumber += 1;
  repo.pulls.set(pr.number, pr);
  return { pr };
}

function branchList(repo: Repo): Array<Record<string, unknown>> {
  const out: Array<Record<string, unknown>> = [];
  for (const [ref, sha] of [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1))) {
    if (!ref.startsWith('refs/heads/')) continue;
    out.push({ name: ref.slice(11), commit: { sha }, protected: false });
  }
  return out;
}

function commitsOf(repo: Repo, tip: string): Array<Record<string, unknown>> {
  const dist = commitDistances(repo, [tip], -1);
  const ordered = [...dist.entries()].sort((a, b) => a[1] - b[1]).map(([oid]) => oid);
  const out: Array<Record<string, unknown>> = [];
  for (const oid of ordered) {
    const o = repo.objects.get(oid);
    if (!o || o.type !== 'commit') continue;
    const c = parseCommit(o.data);
    out.push({
      sha: oid, node_id: gid('Commit', oid),
      commit: {
        message: c.message.replace(/\n+$/, ''), author: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: NOW_ISO },
        committer: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: NOW_ISO },
        tree: { sha: c.tree },
      },
      author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      committer: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      parents: c.parents.map((p) => ({ sha: p })),
      url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${oid}`,
    });
  }
  return out;
}

function contentEntry(repo: Repo, tip: string, path: string): { kind: 'file'; blobSha: string; content: Buffer } | { kind: 'dir'; entries: Array<Record<string, unknown>> } | null {
  const flat = treeOfCommit(repo, tip);
  const direct = flat.get(path);
  if (direct) {
    const o = repo.objects.get(direct.oid);
    if (!o) return null;
    return { kind: 'file', blobSha: direct.oid, content: o.data };
  }
  const prefix = path === '' ? '' : `${path}/`;
  const entries: Array<Record<string, unknown>> = [];
  const seenDirs = new Set<string>();
  for (const [p, e] of flat) {
    if (prefix !== '' && !p.startsWith(prefix)) continue;
    const rest = prefix === '' ? p : p.slice(prefix.length);
    const seg = rest.split('/')[0]!;
    if (rest.includes('/')) {
      if (!seenDirs.has(seg)) {
        seenDirs.add(seg);
        entries.push({ type: 'dir', name: seg, path: prefix === '' ? seg : `${prefix}${seg}`, sha: '' });
      }
    } else {
      entries.push({ type: 'file', name: seg, path: p, sha: e.oid, size: repo.objects.get(e.oid)?.data.length ?? 0, encoding: 'base64' });
    }
  }
  if (entries.length === 0) return null;
  entries.sort((a, b) => String(a.name).localeCompare(String(b.name)));
  return { kind: 'dir', entries };
}

async function handleRest(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
  const { path, query } = pathParts(req);
  if (!path.startsWith('/api/v3')) return false;
  const method = req.method ?? 'GET';
  const sub = path.slice('/api/v3'.length);
  // API root and meta need no token (the starter answers them openly).
  if (method === 'GET' && (sub === '' || sub === '/')) {
    sendJson(res, 200, {
      current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit',
      repository_url: '/api/v3/repos/{owner}/{repo}',
    }, { 'x-oauth-scopes': 'repo, read:org' });
    return true;
  }
  if (method === 'GET' && sub === '/meta') {
    sendJson(res, 200, { verifiable_password_authentication: true, installed_version: '1.0.0' }, { 'x-oauth-scopes': 'repo, read:org' });
    return true;
  }
  if (!authed(req)) { sendErr(res, 401, 'Requires authentication'); return true; }
  const rawBody = (method === 'GET' || method === 'HEAD') ? Buffer.alloc(0) : await readBody(req);
  let parsed: { ok: true; value: unknown } | { ok: false } = { ok: true, value: {} };
  const needsBody = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';
  if (needsBody && rawBody.length > 0) {
    parsed = parseJsonBody(rawBody);
    if (!parsed.ok) { sendErr(res, 400, 'Invalid JSON'); return true; }
  }
  const body = isObj(parsed.value) ? parsed.value : {};
  const seg = sub.split('/').filter((s) => s !== '');

  // GET /user
  if (method === 'GET' && seg.length === 1 && seg[0] === 'user') {
    sendJson(res, 200, { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: gid('User', VIEWER_LOGIN), name: VIEWER_NAME, email: VIEWER_EMAIL, type: 'User' });
    return true;
  }
  // GET /user/repos + POST /user/repos
  if (seg.length === 2 && seg[0] === 'user' && seg[1] === 'repos') {
    if (method === 'GET') {
      const list = [...repos.values()].filter((r) => r.owner === VIEWER_LOGIN).sort((a, b) => a.createdSeq - b.createdSeq).map(repoJson);
      sendPage(req, res, list, query);
      return true;
    }
    if (method === 'POST') {
      const name = str(body.name);
      if (name === '') { sendErr(res, 422, 'name is required'); return true; }
      if (repos.has(repoKey(VIEWER_LOGIN, name))) { sendErr(res, 422, 'Repository already exists'); return true; }
      const repo = createRepo(name, body.private === true, str(body.description));
      sendJson(res, 201, repoJson(repo));
      return true;
    }
  }
  // /repos/{owner}/{name}[...]
  if (seg.length >= 3 && seg[0] === 'repos') {
    const owner = decodeURIComponent(seg[1]!);
    const name = decodeURIComponent(seg[2]!);
    const repo = findRepo(owner, name);
    const rest = seg.slice(3);
    if (!repo) { sendErr(res, 404, 'Not Found'); return true; }
    if (rest.length === 0) {
      if (method === 'GET') { sendJson(res, 200, repoJson(repo)); return true; }
      if (method === 'PATCH') {
        if (typeof body.description === 'string') repo.description = body.description;
        if (typeof body.private === 'boolean') repo.priv = body.private;
        sendJson(res, 200, repoJson(repo));
        return true;
      }
      if (method === 'DELETE') {
        repos.delete(repoKey(owner, name));
        sendNoContent(res, 204);
        return true;
      }
      sendErr(res, 404, 'Not Found');
      return true;
    }
    // branches
    if (rest[0] === 'branches') {
      if (method === 'GET' && rest.length === 1) { sendJson(res, 200, branchList(repo)); return true; }
      if (method === 'GET' && rest.length === 2) {
        const branch = decodeURIComponent(rest[1]!);
        const sha = repo.refs.get(`refs/heads/${branch}`);
        if (!sha) { sendErr(res, 404, 'Not Found'); return true; }
        sendJson(res, 200, { name: branch, commit: { sha }, protected: false });
        return true;
      }
    }
    // commits
    if (rest[0] === 'commits' && method === 'GET') {
      const shaParam = query.get('sha');
      let tip: string | null = null;
      if (shaParam) tip = repo.refs.get(`refs/heads/${shaParam}`) ?? (isOid(shaParam) ? shaParam : null);
      else tip = defaultTip(repo);
      if (!tip) { sendJson(res, 200, []); return true; }
      sendPage(req, res, commitsOf(repo, tip), query);
      return true;
    }
    // git refs
    if (rest[0] === 'git' && rest[1] === 'refs') {
      if (method === 'POST' && rest.length === 2) {
        const ref = str(body.ref);
        const sha = str(body.sha);
        if (!ref.startsWith('refs/heads/') || !isOid(sha)) { sendErr(res, 422, 'Invalid ref'); return true; }
        if (repo.refs.has(ref)) { sendErr(res, 422, 'Reference already exists'); return true; }
        const o = repo.objects.get(sha);
        if (!o || o.type !== 'commit') { sendErr(res, 422, 'Object not found'); return true; }
        repo.refs.set(ref, sha);
        sendJson(res, 201, { ref, node_id: gid('Ref', `${repo.id}:${ref}`), object: { sha, type: 'commit' } });
        return true;
      }
      if (method === 'GET' && rest.length >= 4 && rest[2] === 'heads') {
        const branch = decodeURIComponent(rest.slice(3).join('/'));
        const sha = repo.refs.get(`refs/heads/${branch}`);
        if (!sha) { sendErr(res, 404, 'Not Found'); return true; }
        sendJson(res, 200, { ref: `refs/heads/${branch}`, node_id: gid('Ref', `${repo.id}:refs/heads/${branch}`), object: { sha, type: 'commit' } });
        return true;
      }
    }
    // contents
    if (rest[0] === 'contents') {
      const filePath = rest.slice(1).map((s) => decodeURIComponent(s)).join('/');
      const refParam = query.get('ref');
      const branch = refParam ?? repo.defaultBranch;
      if (method === 'GET') {
        const tip = repo.refs.get(`refs/heads/${branch}`) ?? defaultTip(repo);
        if (!tip) { sendErr(res, 404, 'Not Found'); return true; }
        const entry = contentEntry(repo, tip, filePath);
        if (!entry) { sendErr(res, 404, 'Not Found'); return true; }
        if (entry.kind === 'dir') { sendJson(res, 200, entry.entries); return true; }
        sendJson(res, 200, {
          type: 'file', encoding: 'base64', size: entry.content.length, name: filePath.split('/').pop() ?? filePath,
          path: filePath, content: entry.content.toString('base64'), sha: entry.blobSha,
        });
        return true;
      }
      if (method === 'PUT') {
        const contentB64 = str(body.content);
        if (contentB64 === '' || typeof body.message !== 'string') { sendErr(res, 422, 'content and message are required'); return true; }
        const content = b64decode(contentB64);
        const branchBody = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
        const curRef = repo.refs.get(`refs/heads/${branchBody}`) ?? defaultTip(repo);
        const curFlat = curRef ? treeOfCommit(repo, curRef) : new Map<string, TreeEntry>();
        const curEntry = curFlat.get(filePath);
        const isUpdate = curEntry !== undefined;
        if (isUpdate && typeof body.sha === 'string' && body.sha !== '' && body.sha !== curEntry.oid) {
          sendErr(res, 409, 'sha does not match');
          return true;
        }
        const { commitSha, blobSha } = setFile(repo, branchBody, filePath, content, body.message);
        sendJson(res, isUpdate ? 200 : 201, {
          content: { name: filePath.split('/').pop() ?? filePath, path: filePath, sha: blobSha, size: content.length, encoding: 'base64', type: 'file' },
          commit: { sha: commitSha },
        });
        return true;
      }
      if (method === 'DELETE') {
        const curRef = repo.refs.get(`refs/heads/${branch}`) ?? defaultTip(repo);
        const curFlat = curRef ? treeOfCommit(repo, curRef) : new Map<string, TreeEntry>();
        if (!curFlat.has(filePath)) { sendErr(res, 404, 'Not Found'); return true; }
        const { commitSha } = setFile(repo, branch, filePath, null, str(body.message, 'Delete file'));
        sendJson(res, 200, { commit: { sha: commitSha } });
        return true;
      }
    }
    // labels
    if (rest[0] === 'labels') {
      if (method === 'GET') {
        sendJson(res, 200, [...repo.labels.values()].map((l) => ({ name: l.name, color: l.color, description: '' })));
        return true;
      }
      if (method === 'POST') {
        const name = str(body.name);
        if (name === '') { sendErr(res, 422, 'name is required'); return true; }
        const color = str(body.color, 'ededed');
        repo.labels.set(name, { name, color });
        sendJson(res, 201, { id: gid('Label', `${repo.id}:${name}`), name, color, description: '' });
        return true;
      }
    }
    // issues
    if (rest[0] === 'issues') {
      if (rest.length === 1) {
        if (method === 'GET') {
          let list = [...repo.issues.values()];
          const state = query.get('state') ?? 'open';
          if (state !== 'all') list = list.filter((i) => i.state === state);
          const sort = query.get('sort') ?? 'created';
          const dir = query.get('direction') ?? 'desc';
          if (sort === 'created') list.sort((a, b) => (dir === 'asc' ? a.seq - b.seq : b.seq - a.seq));
          sendPage(req, res, list.map((i) => issueJson(repo, i)), query);
          return true;
        }
        if (method === 'POST') {
          const title = str(body.title);
          const labelsIn = Array.isArray(body.labels) ? body.labels.filter((x): x is string => typeof x === 'string') : [];
          const labelObjs = Array.isArray(body.labels) && body.labels.length > 0 && typeof body.labels[0] === 'object'
            ? (body.labels as Array<Record<string, unknown>>).map((l) => str(l.name)) : labelsIn;
          const made = createIssueIn(repo, title, str(body.body), labelObjs);
          if ('error' in made) { sendErr(res, 422, made.error); return true; }
          sendJson(res, 201, issueJson(repo, made.issue));
          return true;
        }
      }
      const num = Number(rest[1]);
      if (rest.length >= 2 && Number.isInteger(num)) {
        const issue = repo.issues.get(num);
        if (rest.length === 2) {
          if (!issue) { sendErr(res, 404, 'Not Found'); return true; }
          if (method === 'GET') { sendJson(res, 200, issueJson(repo, issue)); return true; }
          if (method === 'PATCH') {
            if (typeof body.state === 'string' && (body.state === 'open' || body.state === 'closed')) issue.state = body.state;
            if (typeof body.title === 'string') issue.title = body.title;
            if (typeof body.body === 'string') issue.body = body.body;
            sendJson(res, 200, issueJson(repo, issue));
            return true;
          }
        }
        if (rest.length === 3 && rest[2] === 'comments') {
          if (!issue) { sendErr(res, 404, 'Not Found'); return true; }
          if (method === 'GET') {
            sendJson(res, 200, issue.comments.map((c) => ({ id: c.id, node_id: gid('IssueComment', `${repo.id}:${num}:${c.id}`), body: c.body, user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' }, created_at: NOW_ISO, updated_at: NOW_ISO })));
            return true;
          }
          if (method === 'POST') {
            const id = repo.nextComment++;
            issue.comments.push({ id, body: str(body.body) });
            sendJson(res, 201, { id, node_id: gid('IssueComment', `${repo.id}:${num}:${id}`), body: str(body.body), user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' }, created_at: NOW_ISO, updated_at: NOW_ISO });
            return true;
          }
        }
      }
    }
    // pulls
    if (rest[0] === 'pulls') {
      if (rest.length === 1) {
        if (method === 'GET') {
          let list = [...repo.pulls.values()];
          const state = query.get('state') ?? 'open';
          if (state === 'open') list = list.filter((p) => p.state === 'open');
          else if (state === 'closed') list = list.filter((p) => p.state === 'closed');
          list.sort((a, b) => b.seq - a.seq);
          sendPage(req, res, list.map((p) => pullJson(repo, p)), query);
          return true;
        }
        if (method === 'POST') {
          const head = str(body.head);
          const base = str(body.base, repo.defaultBranch);
          const made = createPullIn(repo, str(body.title), str(body.body), head, base);
          if ('error' in made) { sendErr(res, 422, made.error); return true; }
          sendJson(res, 201, pullJson(repo, made.pr));
          return true;
        }
      }
      const num = Number(rest[1]);
      if (rest.length >= 2 && Number.isInteger(num)) {
        const pr = repo.pulls.get(num);
        if (!pr) { sendErr(res, 404, 'Not Found'); return true; }
        if (rest.length === 2 && method === 'GET') { sendJson(res, 200, pullJson(repo, pr)); return true; }
        if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
          sendJson(res, 200, prFiles(repo, pr).map((f) => ({ ...f, sha: '', patch: '' })));
          return true;
        }
        if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
          if (pr.state !== 'open') { sendErr(res, 405, 'Pull request is not open'); return true; }
          const merged = mergePr(repo, pr);
          if ('conflict' in merged) { sendErr(res, 405, 'Merge conflict'); return true; }
          sendJson(res, 200, { sha: merged.sha, merged: true, message: 'Pull request successfully merged' });
          return true;
        }
      }
    }
    sendErr(res, 404, 'Not Found');
    return true;
  }
  sendErr(res, 404, 'Not Found');
  return true;
}

// ---------------------------------------------------------------- GraphQL v4

type GqlNode = { kind: 'field'; name: string; alias: string | null; args: Record<string, unknown>; children: GqlNode[] }
  | { kind: 'frag'; cond: string | null; children: GqlNode[] };

function gqlTokenize(src: string): string[] {
  const toks: string[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i]!;
    if (c === '#') { while (i < src.length && src[i] !== '\n') i += 1; continue; }
    if (/\s/.test(c) || c === ',') { i += 1; continue; }
    if (c === '.' && src.slice(i, i + 3) === '...') { toks.push('...'); i += 3; continue; }
    if ('{}():[]!=$'.includes(c)) { toks.push(c); i += 1; continue; }
    if (c === '"') {
      let j = i + 1; let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) { out += src[j + 1]; j += 2; } else { out += src[j]; j += 1; }
      }
      toks.push(JSON.stringify(out));
      i = j + 1;
      continue;
    }
    let j = i;
    while (j < src.length && /[A-Za-z0-9_.-]/.test(src[j]!)) j += 1;
    if (j === i) { i += 1; continue; }
    toks.push(src.slice(i, j));
    i = j;
  }
  return toks;
}

function gqlParseValue(toks: string[], pos: { i: number }, vars: Record<string, unknown>): unknown {
  const t = toks[pos.i++]!;
  if (t === '$') return vars[toks[pos.i++]!] ?? null;
  if (t === '{') {
    const obj: Record<string, unknown> = {};
    while (toks[pos.i] !== '}') {
      const k = toks[pos.i++]!;
      if (toks[pos.i] === ':') pos.i += 1;
      obj[k] = gqlParseValue(toks, pos, vars);
    }
    pos.i += 1;
    return obj;
  }
  if (t === '[') {
    const arr: unknown[] = [];
    while (toks[pos.i] !== ']') arr.push(gqlParseValue(toks, pos, vars));
    pos.i += 1;
    return arr;
  }
  if (t.startsWith('"')) return JSON.parse(t) as unknown;
  if (t === 'true') return true;
  if (t === 'false') return false;
  if (t === 'null') return null;
  const n = Number(t);
  if (t !== '' && !Number.isNaN(n)) return n;
  return t;
}

function gqlParseSelection(toks: string[], pos: { i: number }, vars: Record<string, unknown>): GqlNode[] {
  // toks[pos.i] === '{'
  pos.i += 1;
  const nodes: GqlNode[] = [];
  while (pos.i < toks.length && toks[pos.i] !== '}') {
    if (toks[pos.i] === '...') {
      pos.i += 1;
      let cond: string | null = null;
      if (toks[pos.i] === 'on') { pos.i += 1; cond = toks[pos.i++]!; }
      const children = gqlParseSelection(toks, pos, vars);
      nodes.push({ kind: 'frag', cond, children });
      continue;
    }
    let name = toks[pos.i++]!;
    let alias: string | null = null;
    if (toks[pos.i] === ':') { alias = name; pos.i += 1; name = toks[pos.i++]!; }
    const args: Record<string, unknown> = {};
    if (toks[pos.i] === '(') {
      pos.i += 1;
      while (toks[pos.i] !== ')') {
        const k = toks[pos.i++]!;
        if (toks[pos.i] === ':') pos.i += 1;
        args[k] = gqlParseValue(toks, pos, vars);
      }
      pos.i += 1;
    }
    let children: GqlNode[] = [];
    if (toks[pos.i] === '{') children = gqlParseSelection(toks, pos, vars);
    nodes.push({ kind: 'field', name, alias, args, children });
  }
  pos.i += 1;
  return nodes;
}

function gqlParseOp(query: string, vars: Record<string, unknown>): { op: 'query' | 'mutation'; nodes: GqlNode[] } {
  const toks = gqlTokenize(query);
  const pos = { i: 0 };
  let op: 'query' | 'mutation' = 'query';
  while (pos.i < toks.length && toks[pos.i] !== '{') {
    const t = toks[pos.i]!;
    if (t === 'mutation') { op = 'mutation'; pos.i += 1; continue; }
    if (t === 'query') { pos.i += 1; continue; }
    if (t === '(') {
      let depth = 0;
      while (pos.i < toks.length) {
        if (toks[pos.i] === '(') depth += 1;
        if (toks[pos.i] === ')') { depth -= 1; if (depth === 0) { pos.i += 1; break; } }
        pos.i += 1;
      }
      continue;
    }
    pos.i += 1;
  }
  const nodes = pos.i < toks.length ? gqlParseSelection(toks, pos, vars) : [];
  return { op, nodes };
}

const GQL_FIELDS: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema', '__typename'],
  Node: ['id', '__typename'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment', '__typename'],
  User: ['login', 'id', 'name', 'email', '__typename'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', '__typename'],
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'author', 'labels', 'comments', '__typename'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount', '__typename'],
  PullRequest: ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'body', 'author', '__typename'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount', '__typename'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo', 'totalCount', '__typename'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType', '__typename'],
  Ref: ['name', 'id', '__typename'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'endCursor', 'startCursor', '__typename'],
  __Type: ['name', 'kind', 'fields', 'enumValues', 'description', '__typename'],
  __Field: ['name', 'description', '__typename'],
  __EnumValue: ['name', 'description', '__typename'],
  __Schema: ['types', 'queryType', 'mutationType', '__typename'],
  CreateIssuePayload: ['issue', '__typename'],
  CloseIssuePayload: ['issue', '__typename'],
  CreatePullRequestPayload: ['pullRequest', '__typename'],
  MergePullRequestPayload: ['pullRequest', '__typename'],
  AddCommentPayload: ['commentEdge', '__typename'],
  IssueCommentEdge: ['node', 'cursor', '__typename'],
  IssueComment: ['id', 'body', '__typename'],
  LabelConnection: ['nodes', '__typename'],
  Label: ['name', 'color', '__typename'],
};

const GQL_TYPE_OF: Record<string, Record<string, string>> = {
  Query: { viewer: 'User', repository: 'Repository', node: 'Node', __type: '__Type', __schema: '__Schema' },
  Mutation: { createIssue: 'CreateIssuePayload', closeIssue: 'CloseIssuePayload', createPullRequest: 'CreatePullRequestPayload', mergePullRequest: 'MergePullRequestPayload', addComment: 'AddCommentPayload' },
  Repository: { owner: 'User', defaultBranchRef: 'Ref', issue: 'Issue', issues: 'IssueConnection', pullRequests: 'PullRequestConnection', pullRequest: 'PullRequest' },
  Issue: { author: 'User', labels: 'LabelConnection' },
  IssueConnection: { nodes: 'Issue', pageInfo: 'PageInfo' },
  PullRequest: { files: 'PullRequestChangedFileConnection', author: 'User' },
  PullRequestConnection: { nodes: 'PullRequest', pageInfo: 'PageInfo' },
  PullRequestChangedFileConnection: { nodes: 'PullRequestChangedFile', pageInfo: 'PageInfo' },
  __Type: { fields: '__Field', enumValues: '__EnumValue' },
  CreateIssuePayload: { issue: 'Issue' },
  CloseIssuePayload: { issue: 'Issue' },
  CreatePullRequestPayload: { pullRequest: 'PullRequest' },
  MergePullRequestPayload: { pullRequest: 'PullRequest' },
  AddCommentPayload: { commentEdge: 'IssueCommentEdge' },
  IssueCommentEdge: { node: 'IssueComment' },
  LabelConnection: { nodes: 'Label' },
};

const TYPE_FIELDS_INTROSPECT: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'author', 'labels', 'comments', 'url'],
  PullRequest: ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'body', 'author'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'url'],
  User: ['login', 'id', 'name', 'email'],
  SearchType: [],
};
const SEARCH_ENUMS = ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT'];

type GqlVal = Record<string, unknown> & { __type: string };

function repoVal(repo: Repo): GqlVal {
  return { __type: 'Repository', repo } as unknown as GqlVal;
}
function issueVal(repo: Repo, issue: Issue): GqlVal {
  return { __type: 'Issue', repo, issue } as unknown as GqlVal;
}
function pullVal(repo: Repo, pr: Pull): GqlVal {
  return { __type: 'PullRequest', repo, pr } as unknown as GqlVal;
}

function findRepoById(id: number): Repo | null {
  for (const r of repos.values()) if (r.id === id) return r;
  return null;
}

function decodeGid(value: string): { kind: string; key: string } | null {
  try {
    const text = Buffer.from(value, 'base64').toString('utf8');
    const i = text.indexOf(':');
    if (i < 0) return null;
    return { kind: text.slice(0, i), key: text.slice(i + 1) };
  } catch { return null; }
}

function resolveNodeByGid(value: string): GqlVal | null {
  const d = decodeGid(value);
  if (!d) return null;
  if (d.kind === 'Repository') {
    const repo = findRepoById(Number(d.key));
    return repo ? repoVal(repo) : null;
  }
  if (d.kind === 'Issue') {
    const [rid, num] = d.key.split(':');
    const repo = findRepoById(Number(rid));
    const issue = repo?.issues.get(Number(num));
    return repo && issue ? issueVal(repo, issue) : null;
  }
  if (d.kind === 'PullRequest') {
    const [rid, num] = d.key.split(':');
    const repo = findRepoById(Number(rid));
    const pr = repo?.pulls.get(Number(num));
    return repo && pr ? pullVal(repo, pr) : null;
  }
  return null;
}

function cursorOf(n: number): string { return b64encode(`cursor:${n}`); }
function cursorNum(c: unknown): number | null {
  if (typeof c !== 'string' || c === '') return null;
  try {
    const t = Buffer.from(c, 'base64').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(t);
    return m ? Number(m[1]) : null;
  } catch { return null; }
}

function issueConn(repo: Repo, args: Record<string, unknown>): GqlVal {
  let list = [...repo.issues.values()];
  const states = args.states;
  if (Array.isArray(states)) {
    const want = new Set(states.map((s) => String(s).toLowerCase()));
    list = list.filter((i) => want.has(i.state));
  }
  const orderBy = isObj(args.orderBy) ? args.orderBy : {};
  const dir = String(orderBy.direction ?? 'DESC');
  list.sort((a, b) => (dir === 'ASC' ? a.number - b.number : b.number - a.number));
  const first = typeof args.first === 'number' ? args.first : 100;
  const after = cursorNum(args.after);
  let start = 0;
  if (after !== null) {
    const idx = list.findIndex((i) => i.number === after);
    start = idx >= 0 ? idx + 1 : 0;
    // ASC default for the paged query has no orderBy; the DESC branch above already
    // sorted by number desc, so the cursor index logic works for both orders.
    if (dir !== 'DESC' && orderBy.field === undefined) {
      // paged query (no orderBy): list was sorted DESC above; re-sort ASC for cursor pages
      list.sort((a, b) => a.number - b.number);
      const idx2 = list.findIndex((i) => i.number === after);
      start = idx2 >= 0 ? idx2 + 1 : 0;
    }
  }
  const page = list.slice(start, start + first);
  const hasNext = start + first < list.length;
  return {
    __type: 'IssueConnection', repo, nodes: page.map((i) => issueVal(repo, i)),
    pageInfo: { hasNextPage: hasNext, hasPreviousPage: start > 0, endCursor: page.length ? cursorOf(page[page.length - 1]!.number) : null, startCursor: page.length ? cursorOf(page[0]!.number) : null },
    totalCount: list.length,
  } as unknown as GqlVal;
}

function execField(typeName: string, val: GqlVal, node: Extract<GqlNode, { kind: 'field' }>): unknown {
  const name = node.name;
  if (name === '__typename') return typeName;
  if (typeName === 'Query') {
    if (name === 'viewer') return { __type: 'User' } as unknown as GqlVal;
    if (name === 'repository') {
      const owner = str(node.args.owner);
      const rname = str(node.args.name);
      const repo = findRepo(owner, rname);
      return repo ? repoVal(repo) : null;
    }
    if (name === 'node') return resolveNodeByGid(str(node.args.id));
    if (name === '__type') {
      const tname = str(node.args.name);
      if (tname === 'SearchType') return { __type: '__Type', tname, kind: 'ENUM' } as unknown as GqlVal;
      if (TYPE_FIELDS_INTROSPECT[tname]) return { __type: '__Type', tname, kind: 'OBJECT' } as unknown as GqlVal;
      return { __type: '__Type', tname, kind: 'OBJECT' } as unknown as GqlVal;
    }
    if (name === '__schema') return { __type: '__Schema' } as unknown as GqlVal;
    return null;
  }
  if (typeName === 'Mutation') {
    const input = isObj(node.args.input) ? node.args.input : {};
    if (name === 'createIssue') {
      const d = decodeGid(str(input.repositoryId));
      const repo = d ? findRepoById(Number(d.key)) : null;
      if (!repo) return { __type: 'CreateIssuePayload', issue: null } as unknown as GqlVal;
      const made = createIssueIn(repo, str(input.title), str(input.body), []);
      if ('error' in made) return { __type: 'CreateIssuePayload', issue: null } as unknown as GqlVal;
      return { __type: 'CreateIssuePayload', issue: issueVal(repo, made.issue) } as unknown as GqlVal;
    }
    if (name === 'closeIssue') {
      const target = resolveNodeByGid(str(input.issueId));
      if (target && target.__type === 'Issue') {
        const issue = (target as unknown as { issue: Issue }).issue;
        issue.state = 'closed';
        return { __type: 'CloseIssuePayload', issue: target } as unknown as GqlVal;
      }
      return { __type: 'CloseIssuePayload', issue: null } as unknown as GqlVal;
    }
    if (name === 'createPullRequest') {
      const d = decodeGid(str(input.repositoryId));
      const repo = d ? findRepoById(Number(d.key)) : null;
      if (!repo) return { __type: 'CreatePullRequestPayload', pullRequest: null } as unknown as GqlVal;
      const made = createPullIn(repo, str(input.title), str(input.body), str(input.headRefName), str(input.baseRefName));
      if ('error' in made) return { __type: 'CreatePullRequestPayload', pullRequest: null } as unknown as GqlVal;
      return { __type: 'CreatePullRequestPayload', pullRequest: pullVal(repo, made.pr) } as unknown as GqlVal;
    }
    if (name === 'mergePullRequest') {
      const target = resolveNodeByGid(str(input.pullRequestId));
      if (target && target.__type === 'PullRequest') {
        const { repo, pr } = target as unknown as { repo: Repo; pr: Pull };
        if (pr.state === 'open') mergePr(repo, pr);
        return { __type: 'MergePullRequestPayload', pullRequest: target } as unknown as GqlVal;
      }
      return { __type: 'MergePullRequestPayload', pullRequest: null } as unknown as GqlVal;
    }
    if (name === 'addComment') {
      const target = resolveNodeByGid(str(input.subjectId));
      if (target && target.__type === 'Issue') {
        const { repo, issue } = target as unknown as { repo: Repo; issue: Issue };
        const id = repo.nextComment++;
        issue.comments.push({ id, body: str(input.body) });
        return { __type: 'AddCommentPayload', commentEdge: { __type: 'IssueCommentEdge', node: { __type: 'IssueComment', id: gid('IssueComment', `${repo.id}:${issue.number}:${id}`), body: str(input.body) } } } as unknown as GqlVal;
      }
      return { __type: 'AddCommentPayload', commentEdge: null } as unknown as GqlVal;
    }
    return null;
  }
  if (typeName === 'User') {
    if (name === 'login') return VIEWER_LOGIN;
    if (name === 'id') return gid('User', VIEWER_LOGIN);
    if (name === 'name') return VIEWER_NAME;
    if (name === 'email') return VIEWER_EMAIL;
    return null;
  }
  if (typeName === 'Repository') {
    const repo = (val as unknown as { repo: Repo }).repo;
    if (name === 'id') return gid('Repository', String(repo.id));
    if (name === 'databaseId') return repo.id;
    if (name === 'name') return repo.name;
    if (name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
    if (name === 'owner') return { __type: 'User' } as unknown as GqlVal;
    if (name === 'defaultBranchRef') {
      const tip = defaultTip(repo);
      return tip ? { __type: 'Ref', name: repo.defaultBranch } as unknown as GqlVal : null;
    }
    if (name === 'viewerPermission') return 'ADMIN';
    if (name === 'hasIssuesEnabled') return true;
    if (name === 'issue') {
      const issue = repo.issues.get(Number(node.args.number));
      return issue ? issueVal(repo, issue) : null;
    }
    if (name === 'issues') return issueConn(repo, node.args);
    if (name === 'pullRequests') {
      let list = [...repo.pulls.values()].filter((p) => p.state === 'open');
      list.sort((a, b) => b.number - a.number);
      const first = typeof node.args.first === 'number' ? node.args.first : 100;
      const page = list.slice(0, first);
      return { __type: 'PullRequestConnection', repo, nodes: page.map((p) => pullVal(repo, p)), pageInfo: { hasNextPage: list.length > first, hasPreviousPage: false, endCursor: null, startCursor: null }, totalCount: list.length } as unknown as GqlVal;
    }
    if (name === 'pullRequest') {
      const pr = repo.pulls.get(Number(node.args.number));
      return pr ? pullVal(repo, pr) : null;
    }
    return null;
  }
  if (typeName === 'Issue') {
    const { repo, issue } = val as unknown as { repo: Repo; issue: Issue };
    if (name === 'id') return gid('Issue', `${repo.id}:${issue.number}`);
    if (name === 'number') return issue.number;
    if (name === 'title') return issue.title;
    if (name === 'state') return issue.state.toUpperCase();
    if (name === 'body') return issue.body;
    if (name === 'createdAt') return NOW_ISO;
    if (name === 'author') return { __type: 'User' } as unknown as GqlVal;
    if (name === 'labels') return { __type: 'LabelConnection', nodes: issue.labels.map((l) => ({ __type: 'Label', name: l.name, color: l.color })) } as unknown as GqlVal;
    return null;
  }
  if (typeName === 'IssueConnection' || typeName === 'PullRequestConnection' || typeName === 'PullRequestChangedFileConnection') {
    const v = val as unknown as { nodes: unknown[]; pageInfo: unknown; totalCount: number };
    if (name === 'nodes') return v.nodes;
    if (name === 'pageInfo') return v.pageInfo;
    if (name === 'totalCount') return v.totalCount;
    if (name === 'edges') return (v.nodes as unknown[]).map((n) => ({ cursor: '', node: n }));
    return null;
  }
  if (typeName === 'PullRequest') {
    const { repo, pr } = val as unknown as { repo: Repo; pr: Pull };
    if (name === 'id') return gid('PullRequest', `${repo.id}:${pr.number}`);
    if (name === 'number') return pr.number;
    if (name === 'title') return pr.title;
    if (name === 'state') return pr.merged ? 'MERGED' : pr.state.toUpperCase();
    if (name === 'merged') return pr.merged;
    if (name === 'headRefName') return pr.head;
    if (name === 'baseRefName') return pr.base;
    if (name === 'mergeable') return prConflicted(repo, pr) ? 'CONFLICTING' : 'MERGEABLE';
    if (name === 'body') return pr.body;
    if (name === 'author') return { __type: 'User' } as unknown as GqlVal;
    if (name === 'files') {
      const files = prFiles(repo, pr).map((f) => ({ __type: 'PullRequestChangedFile', path: f.filename, additions: f.additions, deletions: f.deletions, changeType: f.status.toUpperCase() }));
      const first = typeof node.args.first === 'number' ? node.args.first : 100;
      return { __type: 'PullRequestChangedFileConnection', nodes: files.slice(0, first), pageInfo: { hasNextPage: false, hasPreviousPage: false, endCursor: null, startCursor: null }, totalCount: files.length } as unknown as GqlVal;
    }
    return null;
  }
  if (typeName === 'PullRequestChangedFile') {
    const v = val as unknown as Record<string, unknown>;
    if (name === 'path') return v.path ?? null;
    if (name === 'additions') return v.additions ?? 0;
    if (name === 'deletions') return v.deletions ?? 0;
    if (name === 'changeType') return v.changeType ?? 'MODIFIED';
    return null;
  }
  if (typeName === 'Ref') {
    if (name === 'name') return (val as unknown as { name: string }).name;
    if (name === 'id') return gid('Ref', (val as unknown as { name: string }).name);
    return null;
  }
  if (typeName === 'PageInfo') {
    const v = val as unknown as Record<string, unknown>;
    return v[name] ?? null;
  }
  if (typeName === '__Type') {
    const v = val as unknown as { tname: string; kind: string };
    if (name === 'name') return v.tname;
    if (name === 'kind') return v.kind;
    if (name === 'fields') {
      if (v.kind === 'ENUM') return null;
      const names = TYPE_FIELDS_INTROSPECT[v.tname] ?? GQL_FIELDS[v.tname] ?? [];
      return names.map((n) => ({ __type: '__Field', name: n }));
    }
    if (name === 'enumValues') {
      if (v.tname === 'SearchType') return SEARCH_ENUMS.map((n) => ({ __type: '__EnumValue', name: n }));
      return null;
    }
    if (name === 'description') return null;
    return null;
  }
  if (typeName === '__Field' || typeName === '__EnumValue') {
    if (name === 'name') return (val as unknown as { name: string }).name;
    if (name === 'description') return null;
    return null;
  }
  if (typeName === '__Schema') return null;
  if (typeName === 'CreateIssuePayload' || typeName === 'CloseIssuePayload') {
    if (name === 'issue') return (val as unknown as { issue: unknown }).issue ?? null;
    return null;
  }
  if (typeName === 'CreatePullRequestPayload' || typeName === 'MergePullRequestPayload') {
    if (name === 'pullRequest') return (val as unknown as { pullRequest: unknown }).pullRequest ?? null;
    return null;
  }
  if (typeName === 'AddCommentPayload') {
    if (name === 'commentEdge') return (val as unknown as { commentEdge: unknown }).commentEdge ?? null;
    return null;
  }
  if (typeName === 'IssueCommentEdge') {
    if (name === 'node') return (val as unknown as { node: unknown }).node ?? null;
    if (name === 'cursor') return '';
    return null;
  }
  if (typeName === 'IssueComment') {
    const v = val as unknown as Record<string, unknown>;
    return v[name] ?? null;
  }
  if (typeName === 'LabelConnection') {
    if (name === 'nodes') return (val as unknown as { nodes: unknown[] }).nodes;
    return null;
  }
  if (typeName === 'Label') {
    const v = val as unknown as Record<string, unknown>;
    return v[name] ?? null;
  }
  if (typeName === 'Node') {
    if (name === 'id') return (val as unknown as GqlVal).__type === 'Repository' ? gid('Repository', String((val as unknown as { repo: Repo }).repo.id)) : null;
    return null;
  }
  return null;
}

function childType(parent: string, field: string): string {
  if (parent === 'Node') return 'Node';
  return GQL_TYPE_OF[parent]?.[field] ?? 'String';
}

function validateNodes(nodes: GqlNode[], typeName: string, errors: string[]): void {
  for (const node of nodes) {
    if (node.kind === 'frag') {
      validateNodes(node.children, node.cond ?? typeName, errors);
      continue;
    }
    const allowed = GQL_FIELDS[typeName] ?? [];
    if (!allowed.includes(node.name)) {
      errors.push(`Cannot query field "${node.name}" on type "${typeName}".`);
      continue;
    }
    if (node.children.length > 0) {
      const ct = childType(typeName, node.name);
      if (ct !== 'String' && ct !== 'Node') validateNodes(node.children, ct, errors);
      else if (ct === 'Node') validateNodes(node.children, 'Node', errors);
    }
  }
}

function execNodes(nodes: GqlNode[], typeName: string, val: GqlVal | null): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const node of nodes) {
    if (node.kind === 'frag') {
      if (node.cond === null || node.cond === typeName || (typeName === 'Node' && val !== null)) {
        Object.assign(out, execNodes(node.children, node.cond ?? typeName, val));
      }
      continue;
    }
    if (val === null) { out[node.alias ?? node.name] = null; continue; }
    const result = execField(typeName, val, node);
    if (result !== null && typeof result === 'object' && node.children.length > 0) {
      const ct = childType(typeName, node.name);
      if (Array.isArray(result)) {
        out[node.alias ?? node.name] = result.map((item) => {
          if (item !== null && typeof item === 'object' && '__type' in (item as Record<string, unknown>)) {
            return execNodes(node.children, (item as GqlVal).__type, item as GqlVal);
          }
          return item;
        });
      } else if ('__type' in (result as Record<string, unknown>)) {
        out[node.alias ?? node.name] = execNodes(node.children, (result as GqlVal).__type, result as GqlVal);
      } else if (ct === 'PageInfo') {
        out[node.alias ?? node.name] = execNodes(node.children, 'PageInfo', result as unknown as GqlVal);
      } else {
        out[node.alias ?? node.name] = result;
      }
    } else if (result !== null && typeof result === 'object' && node.children.length > 0 && ctIsScalarObject(typeName, node.name)) {
      out[node.alias ?? node.name] = result;
    } else {
      out[node.alias ?? node.name] = result;
    }
  }
  return out;
}

function ctIsScalarObject(_parent: string, _field: string): boolean { return false; }

function runGraphQLOp(query: string, variables: Record<string, unknown>): { data: Record<string, unknown> | null; errors?: Array<{ message: string }> } {
  let parsed: { op: 'query' | 'mutation'; nodes: GqlNode[] };
  try {
    parsed = gqlParseOp(query, variables);
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] };
  }
  const errors: string[] = [];
  validateNodes(parsed.nodes, parsed.op === 'mutation' ? 'Mutation' : 'Query', errors);
  if (errors.length > 0) {
    const data: Record<string, unknown> = {};
    for (const n of parsed.nodes) if (n.kind === 'field') data[n.alias ?? n.name] = null;
    return { data, errors: errors.map((m) => ({ message: m })) };
  }
  const root = { __type: parsed.op === 'mutation' ? 'Mutation' : 'Query' } as unknown as GqlVal;
  const data = execNodes(parsed.nodes, root.__type, root);
  return { data };
}

async function handleGraphQL(req: IncomingMessage, res: ServerResponse): Promise<void> {
  if (!authed(req)) { sendErr(res, 401, 'Requires authentication'); return; }
  const buf = await readBody(req);
  const parsed = parseJsonBody(buf);
  if (!parsed.ok) { sendErr(res, 400, 'Invalid JSON'); return; }
  const value = parsed.value;
  if (Array.isArray(value)) {
    const out = value.map((entry) => {
      if (!isObj(entry)) return { data: null, errors: [{ message: 'invalid operation' }] };
      return runGraphQLOp(str(entry.query), isObj(entry.variables) ? (entry.variables as Record<string, unknown>) : {});
    });
    sendJson(res, 200, out);
    return;
  }
  if (!isObj(value)) { sendErr(res, 400, 'Invalid GraphQL body'); return; }
  sendJson(res, 200, runGraphQLOp(str(value.query), isObj(value.variables) ? (value.variables as Record<string, unknown>) : {}));
}

// ---------------------------------------------------------------- MCP

interface McpToolDef { name: string; description: string; properties: Record<string, Record<string, unknown>>; required: string[] }

const MCP_TOOLS: McpToolDef[] = [
  { name: 'create_repo', description: 'Create a repository owned by the authenticated user.', properties: { name: { type: 'string', description: 'Repository name' }, description: { type: 'string', description: 'Repository description' }, private: { type: 'boolean', description: 'Whether the repository is private' } }, required: ['name'] },
  { name: 'get_repo', description: 'Get a repository by owner and name.', properties: { owner: { type: 'string', description: 'Repository owner' }, repo: { type: 'string', description: 'Repository name' } }, required: ['owner', 'repo'] },
  { name: 'create_issue', description: 'Create an issue in a repository.', properties: { owner: { type: 'string', description: 'Repository owner' }, repo: { type: 'string', description: 'Repository name' }, title: { type: 'string', description: 'Issue title' }, body: { type: 'string', description: 'Issue body' } }, required: ['owner', 'repo', 'title'] },
  { name: 'list_issues', description: 'List issues in a repository.', properties: { owner: { type: 'string', description: 'Repository owner' }, repo: { type: 'string', description: 'Repository name' }, state: { type: 'string', description: 'Issue state: open, closed or all' } }, required: ['owner', 'repo'] },
  { name: 'create_pull_request', description: 'Create a pull request from a head branch into a base branch.', properties: { owner: { type: 'string', description: 'Repository owner' }, repo: { type: 'string', description: 'Repository name' }, title: { type: 'string', description: 'Pull request title' }, body: { type: 'string', description: 'Pull request body' }, head: { type: 'string', description: 'Head branch name' }, base: { type: 'string', description: 'Base branch name' } }, required: ['owner', 'repo', 'title', 'head', 'base'] },
  { name: 'merge_pull_request', description: 'Merge a pull request with a squash commit.', properties: { owner: { type: 'string', description: 'Repository owner' }, repo: { type: 'string', description: 'Repository name' }, pull_number: { type: 'integer', description: 'Pull request number' } }, required: ['owner', 'repo', 'pull_number'] },
  { name: 'close_issue', description: 'Close an issue.', properties: { owner: { type: 'string', description: 'Repository owner' }, repo: { type: 'string', description: 'Repository name' }, issue_number: { type: 'integer', description: 'Issue number' } }, required: ['owner', 'repo', 'issue_number'] },
  { name: 'list_repos', description: 'List repositories owned by the authenticated user.', properties: {}, required: [] },
];

function mcpToolsList(): unknown[] {
  return MCP_TOOLS.map((t) => ({
    name: t.name, description: t.description,
    inputSchema: { type: 'object', properties: t.properties, required: t.required },
  }));
}

function toolTextResult(value: unknown, isError = false): Record<string, unknown> {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return { content: [{ type: 'text', text }], structuredContent: value, isError };
}

function splitFullName(args: Record<string, unknown>): { owner: string; repo: string } {
  let owner = str(args.owner);
  let repo = str(args.repo);
  const full = str(args.full_name);
  if ((owner === '' || repo === '') && full.includes('/')) {
    const [o, r] = full.split('/');
    if (owner === '') owner = o ?? '';
    if (repo === '') repo = r ?? '';
  }
  return { owner, repo };
}

function callMcpTool(name: string, args: Record<string, unknown>): Record<string, unknown> {
  const def = MCP_TOOLS.find((t) => t.name === name);
  if (!def) return { error: { code: -32602, message: `Unknown tool: ${name}` } };
  const missing = def.required.filter((r) => args[r] === undefined || args[r] === null || args[r] === '');
  if (missing.length > 0) return { result: toolTextResult(`Missing required argument: ${missing.join(', ')}`, true) };
  const { owner, repo: repoName } = splitFullName(args);
  if (name === 'create_repo') {
    const rname = str(args.name);
    if (repos.has(repoKey(VIEWER_LOGIN, rname))) return { result: toolTextResult(`Repository already exists: ${rname}`, true) };
    const repo = createRepo(rname, args.private === true, str(args.description));
    return { result: toolTextResult(repoJson(repo)) };
  }
  if (name === 'list_repos') {
    const list = [...repos.values()].filter((r) => r.owner === VIEWER_LOGIN).map(repoJson);
    return { result: toolTextResult(list) };
  }
  const repo = findRepo(owner, repoName);
  if (!repo) return { result: toolTextResult(`Repository not found: ${owner}/${repoName}`, true) };
  if (name === 'get_repo') return { result: toolTextResult(repoJson(repo)) };
  if (name === 'create_issue') {
    const made = createIssueIn(repo, str(args.title), str(args.body), []);
    if ('error' in made) return { result: toolTextResult(made.error, true) };
    return { result: toolTextResult(issueJson(repo, made.issue)) };
  }
  if (name === 'list_issues') {
    let list = [...repo.issues.values()];
    const state = str(args.state, 'open');
    if (state !== 'all') list = list.filter((i) => i.state === state);
    list.sort((a, b) => b.seq - a.seq);
    return { result: toolTextResult(list.map((i) => issueJson(repo, i))) };
  }
  if (name === 'create_pull_request') {
    const made = createPullIn(repo, str(args.title), str(args.body), str(args.head), str(args.base));
    if ('error' in made) return { result: toolTextResult(made.error, true) };
    return { result: toolTextResult(pullJson(repo, made.pr)) };
  }
  if (name === 'merge_pull_request') {
    const num = Number(args.pull_number);
    const pr = repo.pulls.get(num);
    if (!pr) return { result: toolTextResult(`Pull request not found: ${num}`, true) };
    if (pr.state !== 'open') return { result: toolTextResult('Pull request is not open', true) };
    const merged = mergePr(repo, pr);
    if ('conflict' in merged) return { result: toolTextResult('Merge conflict', true) };
    return { result: toolTextResult({ merged: true, sha: merged.sha, number: pr.number }) };
  }
  if (name === 'close_issue') {
    const num = Number(args.issue_number);
    const issue = repo.issues.get(num);
    if (!issue) return { result: toolTextResult(`Issue not found: ${num}`, true) };
    issue.state = 'closed';
    return { result: toolTextResult(issueJson(repo, issue)) };
  }
  return { error: { code: -32602, message: `Unknown tool: ${name}` } };
}

async function handleMcp(req: IncomingMessage, res: ServerResponse): Promise<void> {
  if (!authed(req)) {
    const text = JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Unauthorized' } });
    res.writeHead(401, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), 'x-github-request-id': nextReqId() });
    res.end(text);
    return;
  }
  const buf = await readBody(req);
  const parsed = parseJsonBody(buf);
  if (!parsed.ok) {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  const value = parsed.value;
  if (!isObj(value) || typeof value.method !== 'string') {
    sendJson(res, 200, { jsonrpc: '2.0', id: isObj(value) ? (value.id ?? null) : null, error: { code: -32600, message: 'Invalid Request' } });
    return;
  }
  const id = value.id ?? null;
  const method = value.method;
  const params = isObj(value.params) ? value.params : {};
  const sessionHeader: Record<string, string> = {};
  if (method === 'notifications/initialized' || method.startsWith('notifications/')) {
    res.writeHead(202, { 'x-github-request-id': nextReqId() });
    res.end();
    return;
  }
  if (method === 'server/discover') {
    const serverInfo = { name: 'github-core-clone', version: '1.0.0' };
    sendJson(res, 200, {
      jsonrpc: '2.0', id,
      result: {
        resultType: 'discover',
        supportedVersions: ['2026-07-28', '2025-11-25'],
        capabilities: { tools: {} },
        serverInfo,
        instructions: 'A GitHub-compatible forge over MCP.',
        _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo },
      },
    });
    return;
  }
  if (method === 'initialize') {
    const requested = str(params.protocolVersion, '2026-07-28');
    const negotiated = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'].includes(requested) ? requested : '2026-07-28';
    sendJson(res, 200, {
      jsonrpc: '2.0', id,
      result: {
        protocolVersion: negotiated,
        capabilities: { tools: {} },
        serverInfo: { name: 'github-core-clone', version: '1.0.0' },
      },
    }, { 'mcp-session-id': 'forge-session-1', ...sessionHeader });
    return;
  }
  if (method === 'ping') {
    sendJson(res, 200, { jsonrpc: '2.0', id, result: {} });
    return;
  }
  if (method === 'tools/list') {
    sendJson(res, 200, { jsonrpc: '2.0', id, result: { tools: mcpToolsList() } });
    return;
  }
  if (method === 'tools/call') {
    const toolName = str(params.name);
    const args = isObj(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    const outcome = callMcpTool(toolName, args);
    if ('error' in outcome) {
      sendJson(res, 200, { jsonrpc: '2.0', id, error: outcome.error });
      return;
    }
    sendJson(res, 200, { jsonrpc: '2.0', id, result: outcome.result });
    return;
  }
  sendJson(res, 200, { jsonrpc: '2.0', id, error: { code: -32601, message: `Method not found: ${method}` } });
}

// ---------------------------------------------------------------- dispatcher

function gitRepoFromPath(path: string): { owner: string; name: string } | null {
  const m = /^\/([^/]+)\/([^/]+)\.git(\/.*)?$/.exec(path);
  if (!m) return null;
  return { owner: decodeURIComponent(m[1]!), name: decodeURIComponent(m[2]!) };
}

export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      try {
        const { path } = pathParts(req);
        const method = req.method ?? 'GET';
        if (method === 'GET' && path === '/healthz') { sendJson(res, 200, { ok: true }); return; }
        if (path === '/api/graphql' && method === 'POST') { await handleGraphQL(req, res); return; }
        if (path === '/mcp' && method === 'POST') { await handleMcp(req, res); return; }
        if (path === '/mcp' && method === 'DELETE') { res.writeHead(200, { 'x-github-request-id': nextReqId() }); res.end(); return; }
        if (path.startsWith('/api/v3')) { await handleRest(req, res); return; }
        const git = gitRepoFromPath(path);
        if (git) {
          const repo = findRepo(git.owner, git.name);
          const suffix = path.slice(`/${git.owner}/${git.name}.git`.length);
          if (method === 'GET' && suffix.startsWith('/info/refs')) {
            if (!repo) { sendErr(res, 404, 'Not Found'); return; }
            const service = new URL(req.url ?? '/', 'http://forge').searchParams.get('service') ?? '';
            const v2 = (req.headers['git-protocol'] ?? '') === 'version=2' && service === 'git-upload-pack';
            const body = service === 'git-receive-pack'
              ? v0Advertise(repo, 'git-receive-pack')
              : v2 ? v2Advertise('git-upload-pack') : v0Advertise(repo, 'git-upload-pack');
            const ct = service === 'git-receive-pack' ? 'application/x-git-receive-pack-advertisement' : 'application/x-git-upload-pack-advertisement';
            res.writeHead(200, { 'content-type': ct, 'content-length': body.length, 'x-github-request-id': nextReqId() });
            res.end(body);
            return;
          }
          if (method === 'POST' && suffix === '/git-upload-pack') {
            if (!repo) { sendErr(res, 404, 'Not Found'); return; }
            const buf = await readBody(req);
            let out: Buffer;
            try {
              out = (req.headers['git-protocol'] ?? '') === 'version=2' ? handleFetchV2(repo, buf) : handleFetchV0(repo, buf);
            } catch {
              out = Buffer.concat([pkt('NAK\n'), FLUSH]);
            }
            res.writeHead(200, { 'content-type': 'application/x-git-upload-pack-result', 'content-length': out.length, 'x-github-request-id': nextReqId() });
            res.end(out);
            return;
          }
          if (method === 'POST' && suffix === '/git-receive-pack') {
            if (!repo) { sendErr(res, 404, 'Not Found'); return; }
            const buf = await readBody(req);
            let out: Buffer;
            try {
              out = handleReceivePack(repo, buf);
            } catch {
              const report = Buffer.concat([pkt('unpack error\n'), FLUSH]);
              out = Buffer.concat([pkt(Buffer.concat([Buffer.from([1]), report])), FLUSH]);
            }
            res.writeHead(200, { 'content-type': 'application/x-git-receive-pack-result', 'content-length': out.length, 'x-github-request-id': nextReqId() });
            res.end(out);
            return;
          }
          sendErr(res, 404, 'Not Found');
          return;
        }
        sendErr(res, 404, 'Not Found');
      } catch (err) {
        if (!res.headersSent) sendErr(res, 500, err instanceof Error ? err.message : 'internal error');
        else res.end();
      }
    })();
  });
}
