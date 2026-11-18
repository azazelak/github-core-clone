# github-core-clone

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/azazelak/github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/azazelak/github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone Git forge in a single Node process: git smart HTTP, a GitHub
REST v3 API, a GraphQL v4 API, and an MCP endpoint, all over one shared
in-memory store. No dependencies, no git binary, no network access, no UI.

This is an entry for the make.fun `github-core-clone-01` challenge
($500, deadline 2026-11-21 06:30 UTC).

## Run

```sh
# Node 22+ (uses --experimental-strip-types; no build step, no install step)
MAKEFUN_SHOP_TOKEN=<token> MAKEFUN_ENV_URL=<env-url> node --experimental-strip-types main.ts
```

`main.ts` binds port 0, POSTs `{"port": n}` to `$MAKEFUN_ENV_URL/register`,
then polls GET `/done` until it answers `{"done": true}` and exits 0.
Without `MAKEFUN_ENV_URL` it exits 1.

For local grading, start the server from `app.ts` (see `BUILD_LOG.md` for the
exact command) and run the kit's `sdk/run.ts` against it; `main.ts` itself
needs a real env URL and exits 1 without `MAKEFUN_ENV_URL`.

## Surfaces

- Git smart HTTP `/{owner}/{repo}.git` — upload-pack protocol v2
  (`Git-Protocol: version=2`, `ls-refs`, `fetch`, shallow/deepen) and
  receive-pack v0 (`report-status`, `side-band-64k`, `delete-refs`).
  Packfiles are parsed and generated in-process (no deltas on write;
  OFS/REF deltas are understood on read). Fast-forward only: a non-FF
  push is refused per ref with `non-fast-forward` and the ref does not move.
- REST v3 `/api/v3` — repos, branches, commits, contents, labels, issues,
  comments, pulls, files, squash merge. Pagination via `Link: rel="next"`;
  every reply carries `X-GitHub-Request-Id` and is JSON.
- GraphQL POST `/api/graphql` — `viewer`, `repository`, `node`, `__type`
  introspection, issue/pull-request connections with cursor pagination,
  mutations, and batched operation arrays.
- MCP POST `/mcp` — JSON-RPC 2.0 Streamable HTTP: `server/discover`
  (2026-07-28), `initialize`, `tools/list`, `tools/call` (8 tools, same
  store as REST/GraphQL).

## Determinism

Nothing in the stored state depends on wall-clock time or randomness:
commit timestamps come from a fixed epoch plus a per-repo counter, REST
timestamps from `MAKEFUN_TRIAL_NOW` (or a fixed fallback), global ids are
base64 of stable `Kind:key` pairs, and request ids come from a counter.
Replayed trials therefore produce byte-identical state.

## Layout

- `main.ts` — process lifecycle (bind, register, done-poll, exit).
- `app.ts` — everything else: git codec, store, HTTP plumbing, git smart
  HTTP, REST v3, GraphQL, MCP, dispatcher.
- `package.json`, `tsconfig.json` — module, no dependencies.
- `LICENSE` — MIT.
- `.makefun/owner.json` — wallet owner record.
- `BUILD_LOG.md` — what was built and the local grading results.
