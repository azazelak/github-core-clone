# BUILD_LOG_GO — GitHub Core Clone, Go rewrite

Rewrite from scratch in Go (stdlib only) of the Node v1 forge, targeting
perf. v1 scored 994/1000 locally (feature 600, agent 200, perf 194) and
940/1000 on dev (perf 140, p50 458.488 µs).

## Build

- Toolchain: Go 1.25.4 (`~/go-tool/go/bin/go`)
- Command: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" -o forge .`
- `go vet ./...` clean. `gofmt` clean.
- Binary: `forge`, 6.332.600 byte (~6,0 MiB), ELF 64-bit LSB, statically
  linked, stripped. Under the 8 MiB/file cap; whole entry ~6,4 MiB, under
  the 16 MiB total cap.
- `go.mod`: no dependencies. No `go.sum`.

## Design (perf)

- Hand-rolled HTTP/1.1 on `net` (goroutine per connection, keep-alive,
  Content-Length + chunked bodies, `Expect: 100-continue` answered before
  reading the body). `sync.Pool` for bufio reader/writer; `sync.Pool` for
  zlib writers.
- One store mutex; handlers build the full response under the lock, the
  HTTP layer writes after unlocking.
- Pack cache keyed by repo state version + wants/haves/shallow/depth
  (FIFO, 128 entries); `repo.Ver` bumps on every mutation (receive-pack
  merges objects even when all ref updates are refused, and still bumps).
- Issue-JSON cache keyed by (repo, number, repo.Ver).
- Pack write is delta-free with zlib BestSpeed; pack read resolves OFS
  (type 6) and REF (type 7) deltas; inflated bytes counted exactly via a
  counting reader under `zlib.NewReader` (verified: 81/81 bytes consumed
  for an 81-byte stream).
- Determinism: no `time.Now`/`rand` in state; commit timestamps are
  1700000000 + per-repo counter; REST stamps come from
  `MAKEFUN_TRIAL_NOW` (fallback `2025-01-01T00:00:00.000Z`); ids and
  request ids come from counters.

## Grading (kit runner, fresh server each run)

Command (each run, fresh `./forge`):
`node --experimental-strip-types <kit>/sdk/run.ts --url http://127.0.0.1:<PORT> --token test-token --perf --json`

### Run 1 — 2026-11-19 ~08:20 WIB

- Checks: **120/120** (0 failing)
- Columns: feature **600**, agent **200**, perf **200**, score **1000**, working 1
- perf_p50_us: **154.154**
- push: p50 178.753 ms, 50 pts (rounds 280.93 / 178.753 / 157.639)
- clone: p50 66.943 ms, 50 pts (rounds 68.287 / 66.943 / 50.327)
- read: p50 528.995 ms, 50 pts (rounds 528.995 / 547.599 / 381.802)
- write: p50 89.21 ms, 50 pts (rounds 96.458 / 89.21 / 75.794)

### Run 2 — 2026-11-19 ~08:21 WIB (fresh server)

- Checks: **120/120** (0 failing)
- Columns: feature **600**, agent **200**, perf **199**, score **999**, working 1
- perf_p50_us: **149.044**
- push: p50 194.536 ms, 50 pts (rounds 249.164 / 194.536 / 177.862)
- clone: p50 55.881 ms, 50 pts (rounds 73.357 / 55.881 / 50.96)
- read: p50 497.286 ms, 50 pts (rounds 574.431 / 497.286 / 381.211)
- write: p50 91.283 ms, 49 pts (rounds 91.283 / 96.659 / 71.059)

Gate (120/120 both runs, perf ≥196, p50 well below v1's 177.823 µs,
clone 50/50): **MET**. Local score 1000 / 999 vs v1's 994. p50 is
~13–16% below v1 locally; the bench is client-bound on this VM, so the
local margin understates the dev-instance gain (v1 dev p50 was 458.488 µs
vs 177.823 µs local — the dev instance is ~2,6x slower, where server-side
savings matter more).

## Real git CLI sanity (git 2.43.0, fresh server)

- Empty clone: ok (`warning: You appear to have cloned an empty repository.`)
- Push (2 commits, delta pack): exit 0
- Full clone: 2 commits, file contents correct
- `git clone --depth 1`: 1 commit, contents correct
- `git -c protocol.version=0 clone`: 2 commits
- Second push, then fresh clone: new content (`NEW-CONTENT-XYZ`) present,
  3 commits — pack-cache invalidation proven

## Launcher (`main.ts`, Node 22 stdlib only)

- Copies `./forge` to a tmp file, chmod 755, spawns with inherited
  stdio/env, forwards SIGINT/SIGTERM/SIGHUP, exits with the child's code.
- Verified with `forge` chmod 644 (no exec bit): healthz + REST answer
  through the launcher; SIGTERM to the launcher exits it and the child.
- `main.ts` binds no port.

## Files

- `main.go` — lifecycle (bind :0, POST register, poll /done, exit 0)
- `http.go` — HTTP/1.1 server + dispatcher
- `store.go` — store, graph walks, contents/PR logic
- `pack.go` — pkt-lines, pack write/read, delta apply
- `gitproto.go` — smart HTTP (v2/v0 upload-pack, v0 receive-pack)
- `rest.go` — REST v3
- `graphql.go` — GraphQL v4
- `mcp.go` — MCP JSON-RPC 2.0
- `main.ts` — Node 22 launcher
- `go.mod` — module `forge`, no deps
- `forge` — prebuilt static binary
- `README.md`, `LICENSE`, `.makefun/owner.json` (unchanged)

## Independent re-verification by parent (2026-11-19 ~15:25 WIB)

Fresh servers, same kit runner `--perf --json`:
- Run P1 (through the `main.ts` launcher, dummy env): **120/120**, perf **195**
  (push 50, clone 49, read 50, write 46), p50 **171.874 µs**, total **995**.
- Run P2 (direct `./forge`, fresh server): **120/120**, perf **198**
  (push 50, clone 50, read 50, write 48), p50 **155.951 µs**, total **998**.
- Real git CLI (2.43.0) against a fresh server: empty clone, push, full clone
  (content correct), `--depth 1` clone (1 commit) — all pass. (One initial
  clone failed only because the test used the wrong owner in the URL; the
  forge correctly 404s a repo under another owner.)
- Combined with the builder's two runs (200/154.154 µs, 199/149.044 µs), Go
  grades 195–200 locally vs v1's 192–194 — consistently faster, clone 50/50
  in 3 of 4 runs (v1: 45).

## Status

NOT pushed, NOT submitted, NOT bundled. Ready for parent: push + bundle
+ submit when instructed.
