// Git smart HTTP: protocol v2 upload-pack (ls-refs/fetch, shallow/deepen),
// protocol v0 upload-pack and receive-pack (report-status, side-band-64k,
// delete-refs), fast-forward only. Pack results are cached per repo state
// version + the full request arguments, so repeated identical clones serve
// prebuilt bytes; every mutation bumps repo.Ver and invalidates the cache.
package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

func sortedRefs(r *repo) [][2]string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, n := range names {
		out = append(out, [2]string{n, r.Refs[n]})
	}
	return out
}

func v0Advertise(r *repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k multi_ack_detailed thin-pack ofs-delta agent=github-core-clone/1.0 symref=HEAD:refs/heads/main shallow"
	} else {
		caps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0 symref=HEAD:refs/heads/main"
	}
	var buf bytes.Buffer
	buf.Write(pktStr("# service=" + service + "\n"))
	buf.Write(flushPkt)
	refs := sortedRefs(r)
	headTip := r.Refs["refs/heads/main"]
	if len(refs) == 0 {
		buf.Write(pktStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		if headTip != "" {
			buf.Write(pktStr(headTip + " HEAD\x00" + caps + "\n"))
		}
		for _, rf := range refs {
			buf.Write(pktStr(rf[1] + " " + rf[0] + "\n"))
			if o, ok := r.Objects[rf[1]]; ok && o.typ == objTag {
				buf.Write(pktStr(r.peelOID(rf[1]) + " " + rf[0] + "^{}\n"))
			}
		}
	}
	buf.Write(flushPkt)
	return buf.Bytes()
}

func v2Advertise() []byte {
	lines := []string{
		"version 2\n",
		"agent=github-core-clone/1.0\n",
		"ls-refs=unborn\n",
		"fetch=shallow wait-for-done filter\n",
		"object-format=sha1\n",
		"object-info\n",
	}
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(pktStr(l))
	}
	buf.Write(flushPkt)
	return buf.Bytes()
}

func lsRefsResponse(r *repo, prefixes []string, peel, symrefs, unborn bool) []byte {
	var buf bytes.Buffer
	refs := sortedRefs(r)
	headTip := r.Refs["refs/heads/main"]
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if len(refs) == 0 {
		if unborn && keep("HEAD") {
			buf.Write(pktStr("unborn HEAD symref-target:refs/heads/main\n"))
		}
	} else {
		if keep("HEAD") && symrefs && headTip != "" {
			buf.Write(pktStr(headTip + " HEAD symref-target:refs/heads/main\n"))
		}
		for _, rf := range refs {
			if !keep(rf[0]) {
				continue
			}
			line := rf[1] + " " + rf[0]
			if peel {
				if o, ok := r.Objects[rf[1]]; ok && o.typ == objTag {
					line += " peeled:" + r.peelOID(rf[1])
				}
			}
			buf.Write(pktStr(line + "\n"))
		}
	}
	buf.Write(flushPkt)
	return buf.Bytes()
}

// heldFromHaves: objects the client already holds. A shallow boundary
// commit is held itself, but its ancestors are NOT held.
func (r *repo) heldFromHaves(haves, shallowIn []string) map[string]bool {
	boundary := make(map[string]bool, len(shallowIn))
	for _, s := range shallowIn {
		boundary[s] = true
	}
	held := make(map[string]bool)
	stack := append(append([]string{}, haves...), shallowIn...)
	seen := make(map[string]bool)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		o, ok := r.Objects[oid]
		if !ok {
			continue
		}
		if o.typ == objCommit {
			held[oid] = true
			for x := range r.reachable([]string{o.commitTree()}) {
				held[x] = true
			}
			if !boundary[oid] {
				stack = append(stack, o.commitParents()...)
			}
		} else {
			for x := range r.reachable([]string{oid}) {
				held[x] = true
			}
		}
	}
	return held
}

type fetchPlan struct {
	send      map[string]bool
	shallow   []string
	unshallow []string
}

func (r *repo) planFetch(wants, haves, shallowIn []string, depth int) fetchPlan {
	// depth < 0 means no deepen.
	extraRoots := []string{}
	direct := make(map[string]bool)
	for _, w := range wants {
		if o, ok := r.Objects[w]; ok && o.typ == objTag {
			direct[w] = true
			extraRoots = append(extraRoots, r.peelOID(w))
		}
	}
	roots := append(append([]string{}, wants...), extraRoots...)
	maxDepth := -1
	if depth >= 0 {
		maxDepth = depth - 1
	}
	dist := r.commitDistances(roots, maxDepth)
	included := make(map[string]bool)
	for oid, d := range dist {
		if depth < 0 || d < depth {
			included[oid] = true
		}
	}
	send := make(map[string]bool)
	for oid := range included {
		o, ok := r.Objects[oid]
		if !ok || o.typ != objCommit {
			continue
		}
		for x := range r.reachable([]string{o.commitTree()}) {
			send[x] = true
		}
		send[oid] = true
	}
	held := r.heldFromHaves(haves, shallowIn)
	for x := range held {
		delete(send, x)
	}
	for d := range direct {
		send[d] = true
	}
	var shallow, unshallow []string
	if depth >= 0 {
		for oid, d := range dist {
			if d == depth-1 {
				if o, ok := r.Objects[oid]; ok && o.typ == objCommit && len(o.commitParents()) > 0 {
					shallow = append(shallow, oid)
				}
			}
		}
		for _, s := range shallowIn {
			if included[s] {
				if o, ok := r.Objects[s]; ok && o.typ == objCommit {
					parents := o.commitParents()
					if len(parents) > 0 {
						allIn := true
						for _, p := range parents {
							if !included[p] {
								allIn = false
							}
						}
						if allIn {
							unshallow = append(unshallow, s)
						}
					}
				}
			}
		}
	}
	return fetchPlan{send: send, shallow: shallow, unshallow: unshallow}
}

func sidebandPack(pack []byte) []byte {
	var buf bytes.Buffer
	for off := 0; off < len(pack); off += 60000 {
		end := off + 60000
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 1, end-off+1)
		chunk[0] = 1
		chunk = append(chunk, pack[off:end]...)
		buf.Write(pktLine(chunk))
	}
	return buf.Bytes()
}

// buildFetchPack returns the pack bytes for a fetch plan, cached on the
// repo state version plus the full request arguments.
func (r *repo) buildFetchPack(plan fetchPlan, cacheKey string) []byte {
	if cached, ok := r.fetchCache[cacheKey]; ok {
		return cached
	}
	oids := make([]string, 0, len(plan.send))
	for oid := range plan.send {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	objs := make([]*gitObj, 0, len(oids))
	for _, oid := range oids {
		if o, ok := r.Objects[oid]; ok {
			objs = append(objs, o)
		}
	}
	pack := writePack(objs)
	if len(r.fetchCache) >= 128 {
		old := r.fetchOrder[0]
		r.fetchOrder = r.fetchOrder[1:]
		delete(r.fetchCache, old)
	}
	r.fetchCache[cacheKey] = pack
	r.fetchOrder = append(r.fetchOrder, cacheKey)
	return pack
}

// fetchResponse returns the complete fetch response bytes for a request
// (acknowledgments, shallow/unshallow lines, sidebanded pack), cached on
// the repo state version plus the full request arguments and the protocol
// flavour. The response is fully determined by those inputs — the pack
// bytes are deterministic (objects are sorted before packing) — so a
// cache hit is byte-identical to a fresh computation, and a clone
// repeated against an unchanged repo skips the whole plan + pack build.
func (r *repo) fetchResponse(proto string, wants, haves, shallowIn []string, depth int, done bool) []byte {
	key := fetchCacheKey(r.Ver, wants, haves, shallowIn, depth) + "|" + proto
	if done {
		key += "|done"
	}
	if cached, ok := r.fetchRespCache[key]; ok {
		return cached
	}
	plan := r.planFetch(wants, haves, shallowIn, depth)
	pack := r.buildFetchPack(plan, fetchCacheKey(r.Ver, wants, haves, shallowIn, depth))
	var buf bytes.Buffer
	if proto == "v2" {
		if !done {
			buf.Write(pktStr("acknowledgments\n"))
			buf.Write(pktStr("NAK\n"))
			buf.Write(pktStr("ready\n"))
			buf.Write(delimPkt)
		}
		buf.Write(pktStr("shallow-info\n"))
	} else {
		buf.Write(pktStr("NAK\n"))
	}
	for _, s := range plan.shallow {
		buf.Write(pktStr("shallow " + s + "\n"))
	}
	for _, s := range plan.unshallow {
		buf.Write(pktStr("unshallow " + s + "\n"))
	}
	if proto == "v2" {
		buf.Write(delimPkt)
		buf.Write(pktStr("packfile\n"))
	}
	buf.Write(sidebandPack(pack))
	buf.Write(flushPkt)
	out := buf.Bytes()
	if len(r.fetchRespCache) >= 128 {
		old := r.fetchRespOrder[0]
		r.fetchRespOrder = r.fetchRespOrder[1:]
		delete(r.fetchRespCache, old)
	}
	r.fetchRespCache[key] = out
	r.fetchRespOrder = append(r.fetchRespOrder, key)
	return out
}

func fetchCacheKey(ver uint64, wants, haves, shallowIn []string, depth int) string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(ver, 10))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(depth))
	b.WriteByte('|')
	b.WriteString(strings.Join(wants, ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(haves, ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(shallowIn, ","))
	return b.String()
}

func parseFetchLines(body []byte) (wants, haves, shallowIn []string, depth int, command string, done bool) {
	depth = -1
	packets := decodePkts(body)
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		t := strings.TrimSpace(string(p.payload))
		switch {
		case strings.HasPrefix(t, "command="):
			command = t[8:]
		case strings.HasPrefix(t, "want "):
			wants = append(wants, strings.TrimSpace(t[5:]))
		case strings.HasPrefix(t, "have "):
			haves = append(haves, strings.TrimSpace(t[5:]))
		case strings.HasPrefix(t, "shallow "):
			shallowIn = append(shallowIn, strings.TrimSpace(t[8:]))
		case strings.HasPrefix(t, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(t[7:])); err == nil {
				depth = n
			}
		case t == "done":
			done = true
		}
	}
	return
}

func handleFetchV2(r *repo, body []byte) []byte {
	wants, haves, shallowIn, depth, command, done := parseFetchLines(body)
	if command == "ls-refs" {
		var prefixes []string
		peel, symrefs, unborn := false, false, false
		for _, p := range decodePkts(body) {
			if p.kind != "data" {
				continue
			}
			t := strings.TrimSpace(string(p.payload))
			switch {
			case t == "peel":
				peel = true
			case t == "symrefs":
				symrefs = true
			case t == "unborn":
				unborn = true
			case strings.HasPrefix(t, "ref-prefix "):
				prefixes = append(prefixes, strings.TrimSpace(t[11:]))
			}
		}
		return lsRefsResponse(r, prefixes, peel, symrefs, unborn)
	}
	return r.fetchResponse("v2", wants, haves, shallowIn, depth, done)
}

func handleFetchV0(r *repo, body []byte) []byte {
	wants, haves, shallowIn, depth, _, _ := parseFetchLines(body)
	// v0 want lines may carry capabilities after a space.
	for i, w := range wants {
		if sp := strings.IndexByte(w, ' '); sp >= 0 {
			wants[i] = w[:sp]
		}
	}
	return r.fetchResponse("v0", wants, haves, shallowIn, depth, false)
}

type pushCmd struct {
	oldOID, newOID, ref string
}

func handleReceivePack(r *repo, body []byte) []byte {
	// Commands end at the first flush packet; everything after it is the pack.
	flushAt := -1
	off := 0
	var cmdBytes [][]byte
	for off+4 <= len(body) {
		h := string(body[off : off+4])
		if !isHex4(h) {
			break
		}
		n := hexVal(h)
		if n == 0 {
			flushAt = off + 4
			break
		}
		if n < 4 || off+n > len(body) {
			break
		}
		cmdBytes = append(cmdBytes, body[off:off+n])
		off += n
	}
	var commands []pushCmd
	for _, raw := range cmdBytes {
		text := string(raw[4:])
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		head := strings.TrimSpace(text)
		parts := strings.Split(head, " ")
		if len(parts) == 3 && isOID(parts[0]) && isOID(parts[1]) {
			commands = append(commands, pushCmd{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		}
	}
	unpackOK := true
	incoming := make(map[string]*gitObj)
	if flushAt >= 0 && flushAt < len(body) {
		parsed, err := readPack(body[flushAt:])
		if err != nil {
			unpackOK = false
		} else {
			incoming = parsed
		}
	}
	var lines []string
	if unpackOK {
		for oid, o := range incoming {
			r.Objects[oid] = o
		}
		r.touch()
		lines = append(lines, "unpack ok")
		for _, cmd := range commands {
			cur, hasCur := r.Refs[cmd.ref]
			switch {
			case cmd.newOID == zeroOID:
				if !hasCur || cur != cmd.oldOID {
					lines = append(lines, "ng "+cmd.ref+" invalid old value")
				} else {
					delete(r.Refs, cmd.ref)
					r.touch()
					lines = append(lines, "ok "+cmd.ref)
				}
			case r.Objects[cmd.newOID] == nil:
				lines = append(lines, "ng "+cmd.ref+" missing object")
			case cmd.oldOID == zeroOID:
				if hasCur {
					lines = append(lines, "ng "+cmd.ref+" already exists")
				} else {
					r.Refs[cmd.ref] = cmd.newOID
					r.touch()
					lines = append(lines, "ok "+cmd.ref)
				}
			case !hasCur || cur != cmd.oldOID:
				lines = append(lines, "ng "+cmd.ref+" invalid old value")
			case !r.isAncestor(cmd.oldOID, cmd.newOID):
				lines = append(lines, "ng "+cmd.ref+" non-fast-forward")
			default:
				r.Refs[cmd.ref] = cmd.newOID
				r.touch()
				lines = append(lines, "ok "+cmd.ref)
			}
		}
	} else {
		lines = append(lines, "unpack error")
		for _, cmd := range commands {
			lines = append(lines, "ng "+cmd.ref+" unpacker error")
		}
	}
	// The report's own pkt-lines end with a flush INSIDE the band-1 data.
	var report bytes.Buffer
	for _, l := range lines {
		report.Write(pktStr(l + "\n"))
	}
	report.Write(flushPkt)
	var parts bytes.Buffer
	rep := report.Bytes()
	for o := 0; o < len(rep); o += 60000 {
		end := o + 60000
		if end > len(rep) {
			end = len(rep)
		}
		chunk := make([]byte, 1, end-o+1)
		chunk[0] = 1
		chunk = append(chunk, rep[o:end]...)
		parts.Write(pktLine(chunk))
	}
	parts.Write(flushPkt)
	return parts.Bytes()
}
