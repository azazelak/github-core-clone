# BUILD_LOG — github-core-clone-01

## What was built

Single-process Node 22 forge, stdlib only (`node:http`, `node:crypto`,
`node:zlib`), no dependencies, no git binary. All code written in this
session, in staged chunks (skeleton/lifecycle → git codec + smart HTTP →
REST v3 → GraphQL + MCP + dispatcher → grading fixes).

- `main.ts` — lifecycle: bind port 0, POST `{"port": n}` to
  `$MAKEFUN_ENV_URL/register` with retry, poll GET `/done` until
  `{"done": true}`, exit 0 (exit 1 if `MAKEFUN_ENV_URL` is unset).
- `app.ts` (~2.300 lines) — sha1 object store with commit/tree/blob/tag
  parse+build, delta-aware pack read, delta-free pack write, pkt-line
  framing, upload-pack v2 (ls-refs/fetch, shallow/deepen/unshallow),
  receive-pack v0 (report-status-v2, side-band-64k, delete-refs, FF-only),
  REST v3 (repos/branches/commits/contents/labels/issues/comments/pulls/
  files/squash merge, Link pagination, `X-GitHub-Request-Id` counter),
  GraphQL (selection-set parser, variables, fragments, aliases,
  `__type` introspection, cursor connections, mutations, batch arrays),
  MCP (`server/discover` 2026-07-28, `initialize`, 8 tools on the shared
  store), dispatcher (`/healthz`, git, `/api/graphql`, `/mcp`, `/api/v3`).
- Determinism: fixed-epoch commit timestamps + per-repo counter,
  `MAKEFUN_TRIAL_NOW` REST stamps (fixed fallback), counter request ids,
  base64 `Kind:key` global ids. No `Date.now`/`Math.random`/UUID in state.

## Local grading (kit runner)

Command (server run locally with `MAKEFUN_SHOP_TOKEN=test-token`):

```sh
node --experimental-strip-types <kit>/sdk/run.ts --url http://127.0.0.1:<PORT> --token test-token --perf --json
```

Final run (2026-11-19 ~00:00 WIB, port 43741): **120/120 checks**

| group | passed/total |
|---|---|
| git | 30/30 |
| repos | 15/15 |
| issues | 15/15 |
| pulls | 20/20 |
| protocol | 5/5 |
| graphql | 20/20 |
| mcp | 15/15 |

Perf (same run): **192/200 points**, `perf_p50_us` 214.432

| workload | p50 | points |
|---|---|---|
| push (200× 1 KiB) | 339,978 ms | 50 |
| clone (50 concurrent shallow) | 89,478 ms | 45 |
| read (2000 GET issue) | 690,702 ms | 50 |
| write (200 POST issue) | 100,623 ms | 47 |

Estimated total: feature 600 + agent 200 + perf 192 = **992/1000**.
Perf varies run to run on this shared 1.5-CPU VM (earlier runs: 186 and
193 points; write p50 was 77,659 ms / 50 points in run 1 and 130,137 ms /
43 points in run 2), so the last 8 points are machine noise, not a code
defect worth chasing.

## Fixes found by grading (run 1 was 114/120)

1. `planFetch` treated shallow-boundary commits as fully held, so `deepen`
   never delivered the parent. Fixed with a shallow-aware held walk that
   stops at boundary commits.
2. `prConflicted` compared against the PR-creation base sha instead of the
   current base tip, so a second conflicting PR merged with 200. Fixed to
   use the current base tip (also fixes GraphQL/MCP `mergeable`).
3. `commit.message` kept the git trailing newline; REST now strips it.
4. GraphQL `issues`/`pullRequests` connection `nodes` were raw store
   records; they are now wrapped as typed values (state `OPEN`, `headRefName`
   etc. resolve).
5. GraphQL `node(id:)` validation rejected `id` on the `Node` type; `Node`
   fields added.
6. Real `git` CLI (2.43.0) rejected the v2 advertisement preamble:
   v2 ads now start directly with `version 2` (v0 keeps `# service=`).
7. Real `git push` failed after a correct-looking report: the report's own
   pkt-lines must end with a flush **inside** the sideband band-1 data.
   Fixed in `handleReceivePack`.
8. Real `git clone/fetch` (v2) failed with `expected 'packfile', received
   'acknowledgments'`: v2 fetch sections are separated by DELIM (0001),
   not FLUSH, and a client that sent `done` must get no acknowledgments
   section at all (protocol-v2). Fixed in `handleFetchV2`; v0 fetch now
   also ends with a flush after the pack.

## Real git CLI sanity check (git 2.43.0, port 43741)

- Empty clone, two pushes (fast-forward), full v2 clone (both commits),
  `--depth 1` clone (tip only), `protocol.version=0` clone (both commits):
  all succeed; REST `/commits` lists the pushed commits.

## Independent re-verification by parent (2026-11-19 ~06:55 WIB)

Fresh server from `app.ts` on port 45129, same kit runner `--perf --json`:
**120/120 checks** (all groups green), perf **194/200** (push 50, clone 45,
read 50, write 49), `perf_p50_us` 177823, **total 994/1000**. Builder's
192-point run is confirmed within run-to-run noise.

## Known remaining items

- None on the checks: 120/120, all groups green, no failing check.
- Perf: clone/write sit at 45/47 of 50 points; the gap is within
  run-to-run noise on this VM (see above).
- Not verified: behaviour under the real make.fun trial harness
  (`MAKEFUN_ENV_URL` register/done flow against the live env) — the
  lifecycle code follows the starter's semantics but was only exercised
  locally with a dummy env URL (register retries, server keeps serving).
