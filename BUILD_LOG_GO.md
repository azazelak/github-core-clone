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

## Ronde 2 — optimasi write/clone (2026-11-19 ~18:00 WIB, optimizer f7a9d2b8)

Target: dev perf 145 → 154+ (top-60 saat ini 954). Baseline v3 lokal
(semua run): perf 183–200, p50 143–205 µs — varians antar-run besar
(VM 1,5 CPU bersama, bench client-bound), jadi perbandingan memakai
median + CPU server (/proc utime+stime selama bench), bukan satu run.

### Profiling (build profil /tmp/forgeprof, pprof + sampler /proc)

Satu run bench penuh (120 checks + 4 workload): wall 5.046 ms,
**server CPU 690 ms (~14% wall)**. Profil CPU (560 ms sampel):
`syscall.Syscall6` 23%, `bufio.Flush` 16% cum, `readRequest` 14% cum,
`readPack` 16% cum (push), `encoding/json` + `mallocgc` sisanya —
tidak ada satu hotspot dominan. Per-workload dev diestimasi dari model
3x lokal: push ~45, clone ~35, read ~34, write ~31 poin (total ~145,
cocok dengan dev 145) — write dan clone paling lemah. Rumus poin:
potong waktu 10% ≈ +1,9 poin/workload; butuh ~+9 poin total.

### Perubahan (semua behavior-preserving, stdlib saja)

- **store.go — cache parse di `gitObj`**: `commitParents()`,
  `commitTree()`, `treeEntries()` parse data objek maksimal sekali
  (commit/tree immutable, semua akses di bawah store mutex). Dipakai
  `commitDistances`, `reachable`, `isAncestor`, `flattenTree`,
  `treeOfCommit`, `planFetch`, `heldFromHaves` — sebelumnya tiap push
  me-re-parse seluruh chain (O(n²) parse per round push).
- **rest.go — `issueJSONFast`**: builder JSON hand-rolled untuk issue
  (ganti map + `json.Marshal` yang me-reflect + sort key). `gid`
  dihitung sekali dan dipakai untuk `id` + `node_id`. `issueJSONObj`
  dipertahankan untuk MCP. Nilai field identik — dibuktikan uji
  ekivalensi semantik (string sulit, closed, label kosong).
- **http.go — fast path**: skip `url.Parse` bila target origin-form
  tanpa `?`/`%`; header disimpan sebagai slice pasangan + `EqualFold`
  (ganti map + `ToLower` per header); `writeResp` tulis langsung ke
  bufio (tanpa `bytes.Buffer`); `X-GitHub-Request-Id` dari counter
  atomik (bukan di bawah store mutex).
- **gitproto.go — cache response fetch utuh**: `fetchResponse`
  meng-cache seluruh byte respons (ack + shallow/unshallow + pack
  sideband) per (Ver, wants, haves, shallowIn, depth, proto, done);
  cache hit melewati `planFetch` + `buildFetchPack` sepenuhnya.
- **pack.go — pool flate reader**: `inflateAt` memakai pooled
  `flate.Reader` + `Reset` (header zlib divalidasi, fallback ke
  `zlib.NewReader` bila header korup / bukan Resetter). Uji ekivalensi:
  byte-exact, offset next tepat, termasuk 2 stream berurutan.
- **main.go — `debug.SetGCPercent(500)`**: live set kecil, GC lebih
  jarang dengan biaya heap sedikit lebih besar.

### Grading r2 (fresh server tiap run)

| Run | Checks | Perf | Total | p50 µs | push | clone | read | write |
|---|---|---|---|---|---|---|---|---|
| r2a | 120/120 | 196 | 996 | 160.202 | 186/50 | 68.9/49 | 492/50 | 104/47 |
| r2b | 120/120 | 198 | 998 | 151.065 | 215/50 | 50.2/50 | 487/50 | 98.8/48 |
| r2c | 120/120 | 198 | 998 | 159.462 | 203/50 | 58.8/50 | 563/50 | 96.2/48 |
| r2x | 120/120 | 198 | 998 | 143.520 | 174/50 | 58.0/50 | 440/50 | 95.4/48 |
| r2y | 120/120 | 200 | 1000 | 132.468 | 178/50 | 58.2/50 | 460/50 | 64.6/50 |

### A/B CPU server (bergantian, biner v3 dari HEAD vs r2)

| | server CPU selama bench | wall | p50 µs |
|---|---|---|---|
| v3a | 660 ms | 4.679 ms | 148.149 |
| r2x | 490 ms | 4.225 ms | 143.520 |
| v3b | 640 ms | 4.350 ms | 144.498 |
| r2y | 510 ms | 4.358 ms | 132.468 |

**Server CPU: v3 ~650 ms → r2 ~500 ms (−23%, konsisten di kedua
pasangan).** Wall p50 −3–5% (dalam noise, searah). Mean per-workload
(semua run v3 vs r2): push −3%, clone ~0% (mean ~60 vs ~59 ms — angka
58 vs 50 di A/B tadi murni noise), read −7%, write ~−5%.

### Gate — TIDAK terpenuhi ketat

Gate yang diminta (120/120 kedua run DAN perf ≥199 DAN p50 ≤145 µs
di kedua run) **tidak tercapai** pada metrik wall-time: dari 5 run r2
hanya r2y yang lolos keduanya (200/132 µs); run lain 196–198.
Tapi itu sebagian artefak varians VM — v3 sendiri juga tidak lolos
gate itu secara konsisten (183–200, p50 143–205 µs). Sinyal yang
bersih adalah CPU server −23%. Perkiraan dev: +2–4 poin perf
(145 → ~147–149), **kemungkinan besar masih di bawah garis top-60
(154)**. Keputusan submit ada di parent: r2 jelas lebih hemat CPU
dan 120/120 di semua 5 run, tanpa regresi terukur — tapi bukan
lompatan 9 poin yang dibutuhkan.

### Real git CLI sanity r2 (git 2.43.0, fresh server)

Empty clone ok, push 1 + push 2 exit 0, full clone 2 commit konten
benar, `--depth 1` 1 commit, `protocol.version=0` 2 commit, push
ketiga + fresh clone menampilkan `NEW-CONTENT-R2` (3 commit) —
invalidasi cache terbukti.

### File yang diubah r2

`store.go`, `rest.go`, `http.go`, `gitproto.go`, `pack.go`,
`main.go`, `forge` (rebuild, 6.344.888 byte — masih < 8 MiB/file).
`go vet` bersih, `gofmt` bersih, uji ekivalensi
(`TestIssueJSONEquiv`, `TestInflateEquiv`) lolos lalu file test
dihapus. Tidak ada push/submit/bundle. Biner v3 (dari HEAD) disimpan
di `/tmp/forge-v3` untuk perbandingan; workdir saat ini berisi r2
(belum di-commit, `git status` menunjukkan 7 file modified).

## Status

v3 (Go) SUBMITTED & SCORED dev 945/1000 — entry final saat ini.
Ronde 2 selesai dan terdokumentasi di atas; menunggu keputusan parent
(submit r2 atau tetap di v3). TIDAK ada push/submit/bundle oleh optimizer.

## Verifikasi independen parent atas r2 (2026-11-19 ~18:15 WIB)

Fresh server, kit runner `--perf --json`:
- Run P1: **120/120**, perf **199** (push 50, clone 50, read 50, write 49),
  p50 **144.161 µs**, total **999**.
- Run P2: **120/120**, perf **200** (semua workload 50/50),
  p50 **131.660 µs**, total **1000**.
- Kedua run lolos gate ketat (perf ≥199 dan p50 ≤145 µs). Gabungan 7 run r2
  (optimizer 5 + parent 2): perf 196–200, p50 131,7–160,2 µs; mean perf
  ~198,4, mean p50 ~146 µs — vs v3 (4 run): perf 195–200, mean ~198,
  mean p50 ~158 µs. Sinyal terbersih: CPU server A/B −23% (650 → 500 ms),
  konsisten di kedua pasangan.
- Keputusan parent: **SUBMIT r2** — jelas lebih cepat di sisi server
  (yang paling menentukan di instance dev, yang jauh lebih server-bound
  daripada VM ini). Perkiraan dev +2–4 poin (145 → ~147–149); masih di
  bawah garis top-60 (154), tapi ini perbaikan nyata tanpa regresi di
  satu pun dari 7 run. Submit dijadwalkan 2026-11-20 07:05 WIB (reset day
  cap UTC); v3 945 tetap fallback sampai r2 scored.
