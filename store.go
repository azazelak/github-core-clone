// Store: one in-memory forge state shared by git smart HTTP, REST v3,
// GraphQL and MCP. All handlers run under store.mu (single mutex); the
// HTTP layer builds the response bytes under the lock and writes them
// after unlocking. Determinism: no wall clock or randomness reaches any
// stored state. Commit timestamps come from a fixed epoch plus a per-repo
// counter; REST stamps come from MAKEFUN_TRIAL_NOW (fixed fallback);
// request ids and global ids come from counters / stable keys.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const viewerLogin = "adaiversen"
const viewerID = 1000
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"
const viewerIdent = viewerName + " <" + viewerEmail + ">"

const commitEpoch = 1700000000
const zeroOID = "0000000000000000000000000000000000000000"
const jsonType = "application/json; charset=utf-8"

var token = os.Getenv("MAKEFUN_SHOP_TOKEN")
var nowISO = trialNowISO()

func trialNowISO() string {
	s := os.Getenv("MAKEFUN_TRIAL_NOW")
	if s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		if t, err := time.Parse("2006-01-02T15:04:05Z07:00", s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	return "2025-01-01T00:00:00.000Z"
}

func sha1hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}

func b64enc(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func b64dec(s string) []byte {
	clean := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, s)
	b, _ := base64.StdEncoding.DecodeString(clean)
	return b
}

func gid(kind, key string) string { return b64enc(kind + ":" + key) }

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- git objects

type objType byte

const (
	objCommit objType = 1
	objTree   objType = 2
	objBlob   objType = 3
	objTag    objType = 4
)

func (t objType) name() string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return ""
}

type gitObj struct {
	typ  objType
	data []byte
	// Lazily filled parse caches (all access is under the store mutex,
	// so no extra synchronisation is needed). Commits and trees are
	// immutable, so a parsed view never goes stale.
	parsed    bool
	cParents  []string
	cTree     string
	entries   []treeEntry
	entriesOK bool
}

// commitParents returns the commit's parent OIDs, parsing the object
// data at most once.
func (o *gitObj) commitParents() []string {
	if o.typ != objCommit {
		return nil
	}
	if !o.parsed {
		c := parseCommit(o.data)
		o.cParents, o.cTree = c.parents, c.tree
		o.parsed = true
	}
	return o.cParents
}

// commitTree returns the commit's tree OID, parsing at most once.
func (o *gitObj) commitTree() string {
	if o.typ != objCommit {
		return ""
	}
	if !o.parsed {
		c := parseCommit(o.data)
		o.cParents, o.cTree = c.parents, c.tree
		o.parsed = true
	}
	return o.cTree
}

// treeEntries returns the tree's entries, parsing at most once.
func (o *gitObj) treeEntries() []treeEntry {
	if o.typ != objTree {
		return nil
	}
	if !o.entriesOK {
		o.entries = parseTree(o.data)
		o.entriesOK = true
	}
	return o.entries
}

func hashObj(t objType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(t.name() + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			break
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			break
		}
		nul += sp + 1
		out = append(out, treeEntry{
			mode: string(data[off:sp]),
			name: string(data[sp+1 : nul]),
			oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return out
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		an := sorted[i].name
		if sorted[i].mode == "40000" {
			an += "/"
		}
		bn := sorted[j].name
		if sorted[j].mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		oid, _ := hex.DecodeString(e.oid)
		buf.WriteString(e.mode + " " + e.name + "\x00")
		buf.Write(oid)
	}
	return buf.Bytes()
}

type commitView struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) commitView {
	var c commitView
	blank := bytes.Index(data, []byte("\n\n"))
	var head, msg []byte
	if blank < 0 {
		head = data
	} else {
		head = data[:blank]
		msg = data[blank+2:]
	}
	c.message = string(msg)
	for _, line := range strings.Split(string(head), "\n") {
		sp := strings.IndexByte(line, ' ')
		var k, v string
		if sp < 0 {
			k, v = line, ""
		} else {
			k, v = line[:sp], line[sp+1:]
		}
		switch k {
		case "tree":
			c.tree = v
		case "parent":
			c.parents = append(c.parents, v)
		case "author":
			c.author = v
		case "committer":
			c.committer = v
		}
	}
	return c
}

func buildCommit(tree string, parents []string, message string, ts int64) []byte {
	who := viewerIdent + " " + strconv.FormatInt(ts, 10) + " +0000"
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + who + "\n")
	b.WriteString("committer " + who + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

type tagView struct {
	object, typ, tag, tagger, message string
}

func parseTag(data []byte) tagView {
	var t tagView
	blank := bytes.Index(data, []byte("\n\n"))
	var head []byte
	if blank < 0 {
		head = data
	} else {
		head = data[:blank]
		t.message = string(data[blank+2:])
	}
	for _, line := range strings.Split(string(head), "\n") {
		sp := strings.IndexByte(line, ' ')
		if sp > 0 {
			switch line[:sp] {
			case "object":
				t.object = line[sp+1:]
			case "type":
				t.typ = line[sp+1:]
			case "tag":
				t.tag = line[sp+1:]
			case "tagger":
				t.tagger = line[sp+1:]
			}
		}
	}
	return t
}

// ---------------------------------------------------------------- store types

type label struct {
	Name  string
	Color string
}

type comment struct {
	ID   int
	Body string
}

type issue struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []label
	Comments []comment
	Seq      int
}

type pull struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Head     string
	Base     string
	HeadSha  string
	BaseSha  string
	Merged   bool
	MergeSha string
	MergedAt string
	Seq      int
}

type repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Priv          bool
	DefaultBranch string
	Objects       map[string]*gitObj
	Refs          map[string]string
	Issues        map[int]*issue
	Pulls         map[int]*pull
	Labels        map[string]*label
	NextNumber    int
	NextComment   int
	CommitSeq     int
	CreatedSeq    int
	// ver bumps on every mutation; caches key off it.
	Ver        uint64
	issueCache map[int]cachedJSON
	fetchCache map[string][]byte
	fetchOrder []string
	// fetchRespCache holds whole fetch responses (framing + pack) keyed
	// the same way as fetchCache; a hit skips plan + pack entirely.
	fetchRespCache map[string][]byte
	fetchRespOrder []string
}

type cachedJSON struct {
	ver uint64
	b   []byte
}

type store struct {
	mu          sync.Mutex
	repos       map[string]*repo
	storeSeq    int
	repoCounter int
}

var st = &store{repos: make(map[string]*repo)}

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *store) createRepo(name string, priv bool, description string) *repo {
	s.repoCounter++
	s.storeSeq++
	r := &repo{
		ID: s.repoCounter + 5000, Owner: viewerLogin, Name: name,
		Description: description, Priv: priv, DefaultBranch: "main",
		Objects: make(map[string]*gitObj), Refs: make(map[string]string),
		Issues: make(map[int]*issue), Pulls: make(map[int]*pull),
		Labels:     make(map[string]*label),
		NextNumber: 1, NextComment: 1, CreatedSeq: s.storeSeq,
		issueCache:     make(map[int]cachedJSON),
		fetchCache:     make(map[string][]byte),
		fetchRespCache: make(map[string][]byte),
	}
	s.repos[repoKey(viewerLogin, name)] = r
	return r
}

// findRepo: repo under another owner is 404, never 403.
func (s *store) findRepo(owner, name string) *repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[repoKey(owner, name)]
}

func (s *store) findRepoByID(id int) *repo {
	for _, r := range s.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (r *repo) touch() { r.Ver++ }

func (r *repo) defaultTip() string {
	if v, ok := r.Refs["refs/heads/"+r.DefaultBranch]; ok {
		return v
	}
	return ""
}

func (r *repo) storeObj(t objType, data []byte) string {
	oid := hashObj(t, data)
	r.Objects[oid] = &gitObj{typ: t, data: data}
	return oid
}

func (r *repo) makeCommit(treeOID string, parents []string, message string) string {
	r.CommitSeq++
	ts := int64(commitEpoch + r.CommitSeq)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	data := buildCommit(treeOID, parents, message, ts)
	return r.storeObj(objCommit, data)
}

// ---------------------------------------------------------------- graph walks

func (r *repo) peelOID(oid string) string {
	cur := oid
	for i := 0; i < 10; i++ {
		o, ok := r.Objects[cur]
		if !ok || o.typ != objTag {
			return cur
		}
		cur = parseTag(o.data).object
	}
	return cur
}

// commitDistances: BFS from roots; map oid -> distance. maxDepth < 0 = unlimited.
func (r *repo) commitDistances(roots []string, maxDepth int) map[string]int {
	dist := make(map[string]int)
	type item struct {
		oid string
		d   int
	}
	queue := []item{}
	for _, root := range roots {
		if isOID(root) {
			queue = append(queue, item{root, 0})
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if _, ok := dist[it.oid]; ok {
			continue
		}
		o, ok := r.Objects[it.oid]
		if !ok || o.typ != objCommit {
			continue
		}
		dist[it.oid] = it.d
		if maxDepth >= 0 && it.d >= maxDepth {
			continue
		}
		for _, p := range o.commitParents() {
			if _, ok := dist[p]; !ok {
				queue = append(queue, item{p, it.d + 1})
			}
		}
	}
	return dist
}

func (r *repo) reachable(roots []string) map[string]bool {
	seen := make(map[string]bool)
	stack := []string{}
	for _, root := range roots {
		if isOID(root) {
			stack = append(stack, root)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		o, ok := r.Objects[oid]
		if !ok {
			continue
		}
		seen[oid] = true
		switch o.typ {
		case objCommit:
			stack = append(stack, o.commitTree())
			stack = append(stack, o.commitParents()...)
		case objTag:
			stack = append(stack, parseTag(o.data).object)
		case objTree:
			for _, e := range o.treeEntries() {
				if e.mode != "160000" {
					stack = append(stack, e.oid)
				}
			}
		}
	}
	return seen
}

func (r *repo) isAncestor(anc, tip string) bool {
	if anc == tip {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == anc {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if o, ok := r.Objects[oid]; ok && o.typ == objCommit {
			stack = append(stack, o.commitParents()...)
		}
	}
	return false
}

func (r *repo) mergeBase(a, b string) string {
	da := r.commitDistances([]string{a}, -1)
	db := r.commitDistances([]string{b}, -1)
	best := ""
	bestD := 1 << 30
	for oid, d := range da {
		if dbd, ok := db[oid]; ok && d+dbd < bestD {
			bestD = d + dbd
			best = oid
		}
	}
	return best
}

func (r *repo) flattenTree(treeOID, prefix string, out map[string]treeEntry) {
	o, ok := r.Objects[treeOID]
	if !ok || o.typ != objTree {
		return
	}
	for _, e := range o.treeEntries() {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			r.flattenTree(e.oid, path, out)
		} else {
			out[path] = e
		}
	}
}

func (r *repo) treeOfCommit(commitOID string) map[string]treeEntry {
	out := make(map[string]treeEntry)
	if o, ok := r.Objects[commitOID]; ok && o.typ == objCommit {
		r.flattenTree(o.commitTree(), "", out)
	}
	return out
}

func (r *repo) rebuildTrees(flat map[string]treeEntry) string {
	type node struct {
		files map[string]treeEntry
		dirs  map[string]*node
	}
	mk := func() *node { return &node{files: make(map[string]treeEntry), dirs: make(map[string]*node)} }
	root := mk()
	paths := make([]string, 0, len(flat))
	for p := range flat {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		e := flat[path]
		segs := strings.Split(path, "/")
		n := root
		for i := 0; i < len(segs)-1; i++ {
			if n.dirs[segs[i]] == nil {
				n.dirs[segs[i]] = mk()
			}
			n = n.dirs[segs[i]]
		}
		n.files[segs[len(segs)-1]] = e
	}
	var build func(n *node) string
	build = func(n *node) string {
		entries := []treeEntry{}
		for name, e := range n.files {
			entries = append(entries, treeEntry{mode: e.mode, name: name, oid: e.oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(child)})
		}
		data := buildTree(entries)
		return r.storeObj(objTree, data)
	}
	return build(root)
}

func (r *repo) setFile(branch, path string, content []byte, hasContent bool, message string) (commitSha string, blobSha string) {
	ref := "refs/heads/" + branch
	tip := r.Refs[ref]
	if tip == "" {
		tip = r.defaultTip()
	}
	flat := make(map[string]treeEntry)
	if tip != "" {
		flat = r.treeOfCommit(tip)
	}
	var blob string
	if hasContent {
		blob = r.storeObj(objBlob, content)
		base := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			base = path[i+1:]
		}
		flat[path] = treeEntry{mode: "100644", name: base, oid: blob}
	} else {
		delete(flat, path)
	}
	treeOID := r.rebuildTrees(flat)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitSha = r.makeCommit(treeOID, parents, message)
	r.Refs[ref] = commitSha
	r.touch()
	return commitSha, blob
}

func lineCount(r *repo, oid string) int {
	o, ok := r.Objects[oid]
	if !ok {
		return 0
	}
	text := string(o.data)
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if strings.HasSuffix(text, "\n") {
		return n
	}
	if n == 0 {
		return 1
	}
	return n + 1
}

type prFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

func (r *repo) prFiles(pr *pull) []prFile {
	base := r.mergeBase(pr.HeadSha, pr.BaseSha)
	baseTree := make(map[string]treeEntry)
	if base != "" {
		baseTree = r.treeOfCommit(base)
	}
	headTree := r.treeOfCommit(pr.HeadSha)
	pathsSet := make(map[string]bool)
	for p := range baseTree {
		pathsSet[p] = true
	}
	for p := range headTree {
		pathsSet[p] = true
	}
	paths := make([]string, 0, len(pathsSet))
	for p := range pathsSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := []prFile{}
	for _, p := range paths {
		b, bok := baseTree[p]
		h, hok := headTree[p]
		switch {
		case bok && !hok:
			n := lineCount(r, b.oid)
			out = append(out, prFile{Filename: p, Status: "removed", Additions: 0, Deletions: n, Changes: n})
		case !bok && hok:
			n := lineCount(r, h.oid)
			out = append(out, prFile{Filename: p, Status: "added", Additions: n, Deletions: 0, Changes: n})
		case bok && hok && b.oid != h.oid:
			a := lineCount(r, h.oid)
			d := lineCount(r, b.oid)
			out = append(out, prFile{Filename: p, Status: "modified", Additions: a, Deletions: d, Changes: a + d})
		}
	}
	return out
}

func (r *repo) prConflicted(pr *pull) bool {
	curTip := r.Refs["refs/heads/"+pr.Base]
	if curTip == "" {
		curTip = pr.BaseSha
	}
	base := r.mergeBase(pr.HeadSha, curTip)
	if base == "" {
		return false
	}
	baseTree := r.treeOfCommit(base)
	headTree := r.treeOfCommit(pr.HeadSha)
	curTree := r.treeOfCommit(curTip)
	pathsSet := make(map[string]bool)
	for p := range baseTree {
		pathsSet[p] = true
	}
	for p := range headTree {
		pathsSet[p] = true
	}
	for p := range curTree {
		pathsSet[p] = true
	}
	oidOf := func(m map[string]treeEntry, p string) string {
		if e, ok := m[p]; ok {
			return e.oid
		}
		return ""
	}
	for p := range pathsSet {
		b, h, c := oidOf(baseTree, p), oidOf(headTree, p), oidOf(curTree, p)
		if h != b && c != b && h != c {
			return true
		}
	}
	return false
}

func (r *repo) mergePR(pr *pull) (string, bool) {
	if r.prConflicted(pr) {
		return "", false
	}
	headTree := r.treeOfCommit(pr.HeadSha)
	treeOID := r.rebuildTrees(headTree)
	baseTip := r.Refs["refs/heads/"+pr.Base]
	if baseTip == "" {
		baseTip = pr.BaseSha
	}
	sha := r.makeCommit(treeOID, []string{baseTip}, "Merge pull request #"+strconv.Itoa(pr.Number)+" from "+pr.Head+"\n\n"+pr.Title)
	r.Refs["refs/heads/"+pr.Base] = sha
	pr.State = "closed"
	pr.Merged = true
	pr.MergeSha = sha
	pr.MergedAt = nowISO
	r.touch()
	return sha, true
}
