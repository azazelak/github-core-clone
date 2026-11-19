# github-core-clone

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/azazelak/github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/azazelak/github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone Git forge in a single process: git smart HTTP, a GitHub
REST v3 API, a GraphQL v4 API, and an MCP endpoint, all over one shared
in-memory store. No dependencies, no git binary, no network access, no UI.

This is an entry for the make.fun `github-core-clone-01` challenge
($500, deadline 2026-11-21 06:30 UTC).

The forge is written in Go (stdlib only) and shipped as a prebuilt static
binary `forge` plus a thin Node 22 launcher `main.ts` that stages and
spawns it. The Go source is committed alongside the binary.

## Run

```sh
# Direct (binary owns the lifecycle: bind port 0, register, poll /done)
MAKEFUN_ENV_URL=http://127.0.0.1:PORT MAKEFUN_SHOP_TOKEN=<token> ./forge

# Via the Node launcher (what the trial runs)
MAKEFUN_ENV_URL=http://127.0.0.1:PORT MAKEFUN_SHOP_TOKEN=<token> node main.ts
```

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -buildvcs=false" -o forge .
```

## Layout

- `main.go` — lifecycle: bind port 0, POST register, poll /done, exit 0.
- `http.go` — hand-rolled HTTP/1.1 server (net, keep-alive, chunked bodies).
- `store.go` — in-memory store: objects, refs, issues, pulls, graph walks.
- `pack.go` — packfile codec (pkt-lines, delta-free pack write, OFS/REF delta read).
- `gitproto.go` — git smart HTTP (upload-pack v2, upload-pack v0, receive-pack v0).
- `rest.go` — REST v3.
- `graphql.go` — GraphQL v4.
- `mcp.go` — MCP JSON-RPC 2.0.
- `main.ts` — Node 22 launcher for the prebuilt `forge` binary.
- `BUILD_LOG_GO.md` — build and grading log.
