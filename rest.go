// REST v3 (/api/v3): repos, branches, commits, contents, labels, issues,
// comments, pulls, files, squash merge. All replies JSON with a non-empty
// X-GitHub-Request-Id; errors are {"message": string}.
package main

import (
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type jmap = map[string]any

func strVal(v any, fallback ...string) string {
	if s, ok := v.(string); ok {
		return s
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return ""
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func parseJSONBody(buf []byte) (jmap, bool) {
	if len(buf) == 0 {
		return jmap{}, true
	}
	var v any
	if err := json.Unmarshal(buf, &v); err != nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	return jmap{}, true
}

// ---------------------------------------------------------------- JSON views

func repoJSON(r *repo) jmap {
	open := 0
	for _, i := range r.Issues {
		if i.State == "open" {
			open++
		}
	}
	return jmap{
		"id": r.ID, "node_id": gid("Repository", strconv.Itoa(r.ID)), "name": r.Name,
		"full_name": r.Owner + "/" + r.Name,
		"owner":     jmap{"login": r.Owner, "id": viewerID, "type": "User"},
		"private":   r.Priv, "description": r.Description, "default_branch": r.DefaultBranch,
		"html_url":   "https://github.com/" + r.Owner + "/" + r.Name,
		"url":        "/api/v3/repos/" + r.Owner + "/" + r.Name,
		"clone_url":  "https://github.com/" + r.Owner + "/" + r.Name + ".git",
		"created_at": nowISO, "updated_at": nowISO, "pushed_at": nowISO,
		"has_issues": true, "has_projects": true, "has_wiki": true, "has_pages": false,
		"forks_count": 0, "stargazers_count": 0, "watchers_count": 0, "open_issues_count": open,
		"license": nil, "topics": []any{}, "visibility": map[bool]string{true: "private", false: "public"}[r.Priv],
	}
}

func issueJSONObj(r *repo, iss *issue) jmap {
	labels := make([]any, 0, len(iss.Labels))
	for _, l := range iss.Labels {
		labels = append(labels, jmap{"id": gid("Label", strconv.Itoa(r.ID)+":"+l.Name), "name": l.Name, "color": l.Color, "description": ""})
	}
	closedAt := any(nil)
	if iss.State == "closed" {
		closedAt = nowISO
	}
	return jmap{
		"id":      gid("Issue", strconv.Itoa(r.ID)+":"+strconv.Itoa(iss.Number)),
		"node_id": gid("Issue", strconv.Itoa(r.ID)+":"+strconv.Itoa(iss.Number)),
		"number":  iss.Number, "title": iss.Title, "body": iss.Body, "state": iss.State,
		"labels":   labels,
		"user":     jmap{"login": viewerLogin, "id": viewerID, "type": "User"},
		"assignee": nil, "assignees": []any{}, "milestone": nil, "locked": false,
		"comments": len(iss.Comments), "created_at": nowISO, "updated_at": nowISO, "closed_at": closedAt,
		"html_url":       "https://github.com/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(iss.Number),
		"url":            "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/" + strconv.Itoa(iss.Number),
		"repository_url": "/api/v3/repos/" + r.Owner + "/" + r.Name,
	}
}

func issueJSON(r *repo, iss *issue) []byte {
	b, _ := json.Marshal(issueJSONObj(r, iss))
	return append(b, '\n')
}

func (r *repo) issueJSONCached(iss *issue) []byte {
	if c, ok := r.issueCache[iss.Number]; ok && c.ver == r.Ver {
		return c.b
	}
	b := issueJSON(r, iss)
	r.issueCache[iss.Number] = cachedJSON{ver: r.Ver, b: b}
	return b
}

func pullJSON(r *repo, pr *pull) jmap {
	closedAt := any(nil)
	if pr.State == "closed" {
		closedAt = nowISO
	}
	mergeSha := any(nil)
	if pr.MergeSha != "" {
		mergeSha = pr.MergeSha
	}
	mergedAt := any(nil)
	if pr.MergedAt != "" {
		mergedAt = pr.MergedAt
	}
	return jmap{
		"id":      gid("PullRequest", strconv.Itoa(r.ID)+":"+strconv.Itoa(pr.Number)),
		"node_id": gid("PullRequest", strconv.Itoa(r.ID)+":"+strconv.Itoa(pr.Number)),
		"number":  pr.Number, "title": pr.Title, "body": pr.Body, "state": pr.State,
		"locked": false, "draft": false,
		"head":   jmap{"label": r.Owner + ":" + pr.Head, "ref": pr.Head, "sha": pr.HeadSha, "user": jmap{"login": viewerLogin}, "repo": repoJSON(r)},
		"base":   jmap{"label": r.Owner + ":" + pr.Base, "ref": pr.Base, "sha": pr.BaseSha, "user": jmap{"login": viewerLogin}, "repo": repoJSON(r)},
		"user":   jmap{"login": viewerLogin, "id": viewerID, "type": "User"},
		"merged": pr.Merged, "mergeable": !r.prConflicted(pr), "merge_commit_sha": mergeSha,
		"merged_at": mergedAt, "closed_at": closedAt,
		"created_at": nowISO, "updated_at": nowISO,
		"html_url": "https://github.com/" + r.Owner + "/" + r.Name + "/pull/" + strconv.Itoa(pr.Number),
		"url":      "/api/v3/repos/" + r.Owner + "/" + r.Name + "/pulls/" + strconv.Itoa(pr.Number),
		"commits":  1, "additions": 0, "deletions": 0, "changed_files": len(r.prFiles(pr)),
	}
}

func createIssueIn(r *repo, title, body string, labels []string) (*issue, string) {
	seen := []label{}
	for _, name := range labels {
		lab, ok := r.Labels[name]
		if !ok {
			return nil, "Label does not exist: " + name
		}
		seen = append(seen, *lab)
	}
	st.storeSeq++
	iss := &issue{
		ID: r.ID*100000 + r.NextNumber, Number: r.NextNumber,
		Title: title, Body: body, State: "open", Labels: seen, Seq: st.storeSeq,
	}
	r.NextNumber++
	r.Issues[iss.Number] = iss
	return iss, ""
}

func createPullIn(r *repo, title, body, head, base string) (*pull, string) {
	headSha, ok1 := r.Refs["refs/heads/"+head]
	baseSha, ok2 := r.Refs["refs/heads/"+base]
	if !ok1 || !ok2 {
		return nil, "head or base branch not found"
	}
	st.storeSeq++
	pr := &pull{
		ID: r.ID*100000 + r.NextNumber, Number: r.NextNumber,
		Title: title, Body: body, State: "open",
		Head: head, Base: base, HeadSha: headSha, BaseSha: baseSha, Seq: st.storeSeq,
	}
	r.NextNumber++
	r.Pulls[pr.Number] = pr
	return pr, ""
}

func branchList(r *repo) []any {
	refs := sortedRefs(r)
	out := []any{}
	for _, rf := range refs {
		if !strings.HasPrefix(rf[0], "refs/heads/") {
			continue
		}
		out = append(out, jmap{"name": rf[0][11:], "commit": jmap{"sha": rf[1]}, "protected": false})
	}
	return out
}

func commitsOf(r *repo, tip string) []any {
	dist := r.commitDistances([]string{tip}, -1)
	oids := make([]string, 0, len(dist))
	for oid := range dist {
		oids = append(oids, oid)
	}
	sort.SliceStable(oids, func(i, j int) bool { return dist[oids[i]] < dist[oids[j]] })
	out := []any{}
	for _, oid := range oids {
		o, ok := r.Objects[oid]
		if !ok || o.typ != objCommit {
			continue
		}
		c := parseCommit(o.data)
		parents := []any{}
		for _, p := range c.parents {
			parents = append(parents, jmap{"sha": p})
		}
		out = append(out, jmap{
			"sha": oid, "node_id": gid("Commit", oid),
			"commit": jmap{
				"message":   strings.TrimRight(c.message, "\n"),
				"author":    jmap{"name": viewerName, "email": viewerEmail, "date": nowISO},
				"committer": jmap{"name": viewerName, "email": viewerEmail, "date": nowISO},
				"tree":      jmap{"sha": c.tree},
			},
			"author":    jmap{"login": viewerLogin, "id": viewerID, "type": "User"},
			"committer": jmap{"login": viewerLogin, "id": viewerID, "type": "User"},
			"parents":   parents,
			"url":       "/api/v3/repos/" + r.Owner + "/" + r.Name + "/commits/" + oid,
		})
	}
	return out
}

type contentResult struct {
	kind    string // "file" | "dir"
	blobSha string
	content []byte
	entries []any
}

func contentEntry(r *repo, tip, path string) *contentResult {
	flat := r.treeOfCommit(tip)
	if direct, ok := flat[path]; ok {
		o, ok := r.Objects[direct.oid]
		if !ok {
			return nil
		}
		return &contentResult{kind: "file", blobSha: direct.oid, content: o.data}
	}
	prefix := ""
	if path != "" {
		prefix = path + "/"
	}
	entries := []any{}
	seenDirs := make(map[string]bool)
	for p, e := range flat {
		if prefix != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p
		if prefix != "" {
			rest = p[len(prefix):]
		}
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg = rest[:i]
		}
		if strings.Contains(rest, "/") {
			if !seenDirs[seg] {
				seenDirs[seg] = true
				p := seg
				if prefix != "" {
					p = prefix + seg
				}
				entries = append(entries, jmap{"type": "dir", "name": seg, "path": p, "sha": ""})
			}
		} else {
			size := 0
			if o, ok := r.Objects[e.oid]; ok {
				size = len(o.data)
			}
			entries = append(entries, jmap{"type": "file", "name": seg, "path": p, "sha": e.oid, "size": size, "encoding": "base64"})
		}
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].(jmap)["name"].(string) < entries[j].(jmap)["name"].(string)
	})
	return &contentResult{kind: "dir", entries: entries}
}

// ---------------------------------------------------------------- pagination

func pageParams(q url.Values) (page, perPage int) {
	perPage = 30
	if s := q.Get("per_page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n != 0 {
			perPage = n
		}
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page = 1
	if s := q.Get("page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n != 0 {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	return
}

func sendPage(rq *req, items []any) *resp {
	page, perPage := pageParams(rq.query)
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	var extra map[string]string
	if start+perPage < len(items) {
		q := rq.query
		q.Set("page", strconv.Itoa(page+1))
		q.Set("per_page", strconv.Itoa(perPage))
		next := "http://" + rq.host + rq.rawPath + "?" + q.Encode()
		extra = map[string]string{"link": "<" + next + ">; rel=\"next\""}
	}
	return jsonResp(200, items[start:end], extra)
}

// ---------------------------------------------------------------- router

func handleRest(rq *req) *resp {
	path := rq.path
	sub := strings.TrimPrefix(path, "/api/v3")
	method := rq.method
	if method == "GET" && (sub == "" || sub == "/") {
		return jsonResp(200, jmap{
			"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit",
			"repository_url": "/api/v3/repos/{owner}/{repo}",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}
	if method == "GET" && sub == "/meta" {
		return jsonResp(200, jmap{"verifiable_password_authentication": true, "installed_version": "1.0.0"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}
	if !authed(rq) {
		return errResp(401, "Requires authentication")
	}
	var body jmap = jmap{}
	if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
		if len(rq.body) > 0 {
			b, ok := parseJSONBody(rq.body)
			if !ok {
				return errResp(400, "Invalid JSON")
			}
			body = b
		}
	}
	seg := []string{}
	for _, s := range strings.Split(sub, "/") {
		if s != "" {
			seg = append(seg, s)
		}
	}

	if method == "GET" && len(seg) == 1 && seg[0] == "user" {
		return jsonResp(200, jmap{"login": viewerLogin, "id": viewerID, "node_id": gid("User", viewerLogin), "name": viewerName, "email": viewerEmail, "type": "User"}, nil)
	}
	if len(seg) == 2 && seg[0] == "user" && seg[1] == "repos" {
		if method == "GET" {
			list := []any{}
			var all []*repo
			for _, r := range st.repos {
				if r.Owner == viewerLogin {
					all = append(all, r)
				}
			}
			sort.Slice(all, func(i, j int) bool { return all[i].CreatedSeq < all[j].CreatedSeq })
			for _, r := range all {
				list = append(list, repoJSON(r))
			}
			return sendPage(rq, list)
		}
		if method == "POST" {
			name := strVal(body["name"])
			if name == "" {
				return errResp(422, "name is required")
			}
			if _, exists := st.repos[repoKey(viewerLogin, name)]; exists {
				return errResp(422, "Repository already exists")
			}
			priv, _ := body["private"].(bool)
			r := st.createRepo(name, priv, strVal(body["description"]))
			return jsonResp(201, repoJSON(r), nil)
		}
	}
	if len(seg) >= 3 && seg[0] == "repos" {
		owner := unescape(seg[1])
		name := unescape(seg[2])
		r := st.findRepo(owner, name)
		rest := seg[3:]
		if r == nil {
			return errResp(404, "Not Found")
		}
		if len(rest) == 0 {
			switch method {
			case "GET":
				return jsonResp(200, repoJSON(r), nil)
			case "PATCH":
				if d, ok := body["description"].(string); ok {
					r.Description = d
				}
				if p, ok := body["private"].(bool); ok {
					r.Priv = p
				}
				return jsonResp(200, repoJSON(r), nil)
			case "DELETE":
				delete(st.repos, repoKey(owner, name))
				return &resp{status: 204, body: []byte{}}
			}
			return errResp(404, "Not Found")
		}
		if rest[0] == "branches" {
			if method == "GET" && len(rest) == 1 {
				return jsonResp(200, branchList(r), nil)
			}
			if method == "GET" && len(rest) == 2 {
				branch := unescape(rest[1])
				sha, ok := r.Refs["refs/heads/"+branch]
				if !ok {
					return errResp(404, "Not Found")
				}
				return jsonResp(200, jmap{"name": branch, "commit": jmap{"sha": sha}, "protected": false}, nil)
			}
		}
		if rest[0] == "commits" && method == "GET" {
			shaParam := rq.query.Get("sha")
			var tip string
			if shaParam != "" {
				if v, ok := r.Refs["refs/heads/"+shaParam]; ok {
					tip = v
				} else if isOID(shaParam) {
					tip = shaParam
				}
			} else {
				tip = r.defaultTip()
			}
			if tip == "" {
				return jsonResp(200, []any{}, nil)
			}
			return sendPage(rq, commitsOf(r, tip))
		}
		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
			if method == "POST" && len(rest) == 2 {
				ref := strVal(body["ref"])
				sha := strVal(body["sha"])
				if !strings.HasPrefix(ref, "refs/heads/") || !isOID(sha) {
					return errResp(422, "Invalid ref")
				}
				if _, exists := r.Refs[ref]; exists {
					return errResp(422, "Reference already exists")
				}
				o, ok := r.Objects[sha]
				if !ok || o.typ != objCommit {
					return errResp(422, "Object not found")
				}
				r.Refs[ref] = sha
				r.touch()
				return jsonResp(201, jmap{"ref": ref, "node_id": gid("Ref", strconv.Itoa(r.ID)+":"+ref), "object": jmap{"sha": sha, "type": "commit"}}, nil)
			}
			if method == "GET" && len(rest) >= 4 && rest[2] == "heads" {
				branch := unescape(strings.Join(rest[3:], "/"))
				sha, ok := r.Refs["refs/heads/"+branch]
				if !ok {
					return errResp(404, "Not Found")
				}
				return jsonResp(200, jmap{"ref": "refs/heads/" + branch, "node_id": gid("Ref", strconv.Itoa(r.ID)+":refs/heads/"+branch), "object": jmap{"sha": sha, "type": "commit"}}, nil)
			}
		}
		if rest[0] == "contents" {
			parts := make([]string, 0, len(rest)-1)
			for _, s := range rest[1:] {
				parts = append(parts, unescape(s))
			}
			filePath := strings.Join(parts, "/")
			branch := rq.query.Get("ref")
			if branch == "" {
				branch = r.DefaultBranch
			}
			if method == "GET" {
				tip := r.Refs["refs/heads/"+branch]
				if tip == "" {
					tip = r.defaultTip()
				}
				if tip == "" {
					return errResp(404, "Not Found")
				}
				entry := contentEntry(r, tip, filePath)
				if entry == nil {
					return errResp(404, "Not Found")
				}
				if entry.kind == "dir" {
					return jsonResp(200, entry.entries, nil)
				}
				base := filePath
				if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
					base = filePath[i+1:]
				}
				return jsonResp(200, jmap{
					"type": "file", "encoding": "base64", "size": len(entry.content),
					"name": base, "path": filePath,
					"content": b64enc(string(entry.content)), "sha": entry.blobSha,
				}, nil)
			}
			if method == "PUT" {
				contentB64 := strVal(body["content"])
				msg, msgOK := body["message"].(string)
				if contentB64 == "" || !msgOK {
					return errResp(422, "content and message are required")
				}
				content := b64dec(contentB64)
				branchBody := r.DefaultBranch
				if b, ok := body["branch"].(string); ok && b != "" {
					branchBody = b
				}
				curRef := r.Refs["refs/heads/"+branchBody]
				if curRef == "" {
					curRef = r.defaultTip()
				}
				var curFlat map[string]treeEntry
				if curRef != "" {
					curFlat = r.treeOfCommit(curRef)
				} else {
					curFlat = make(map[string]treeEntry)
				}
				curEntry, isUpdate := curFlat[filePath]
				if isUpdate {
					if sha, ok := body["sha"].(string); ok && sha != "" && sha != curEntry.oid {
						return errResp(409, "sha does not match")
					}
				}
				commitSha, blobSha := r.setFile(branchBody, filePath, content, true, msg)
				base := filePath
				if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
					base = filePath[i+1:]
				}
				status := 201
				if isUpdate {
					status = 200
				}
				return jsonResp(status, jmap{
					"content": jmap{"name": base, "path": filePath, "sha": blobSha, "size": len(content), "encoding": "base64", "type": "file"},
					"commit":  jmap{"sha": commitSha},
				}, nil)
			}
			if method == "DELETE" {
				curRef := r.Refs["refs/heads/"+branch]
				if curRef == "" {
					curRef = r.defaultTip()
				}
				var curFlat map[string]treeEntry
				if curRef != "" {
					curFlat = r.treeOfCommit(curRef)
				} else {
					curFlat = make(map[string]treeEntry)
				}
				if _, ok := curFlat[filePath]; !ok {
					return errResp(404, "Not Found")
				}
				commitSha, _ := r.setFile(branch, filePath, nil, false, strVal(body["message"], "Delete file"))
				return jsonResp(200, jmap{"commit": jmap{"sha": commitSha}}, nil)
			}
		}
		if rest[0] == "labels" {
			if method == "GET" {
				list := []any{}
				for _, l := range r.Labels {
					list = append(list, jmap{"name": l.Name, "color": l.Color, "description": ""})
				}
				return jsonResp(200, list, nil)
			}
			if method == "POST" {
				name := strVal(body["name"])
				if name == "" {
					return errResp(422, "name is required")
				}
				color := strVal(body["color"], "ededed")
				r.Labels[name] = &label{Name: name, Color: color}
				return jsonResp(201, jmap{"id": gid("Label", strconv.Itoa(r.ID)+":"+name), "name": name, "color": color, "description": ""}, nil)
			}
		}
		if rest[0] == "issues" {
			if len(rest) == 1 {
				if method == "GET" {
					list := []*issue{}
					for _, i := range r.Issues {
						list = append(list, i)
					}
					state := rq.query.Get("state")
					if state == "" {
						state = "open"
					}
					if state != "all" {
						filtered := list[:0]
						for _, i := range list {
							if i.State == state {
								filtered = append(filtered, i)
							}
						}
						list = filtered
					}
					if rq.query.Get("sort") != "" && rq.query.Get("sort") != "created" {
						// v1 leaves insertion (creation) order when sort is
						// not "created".
						sort.SliceStable(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
					} else if rq.query.Get("direction") == "asc" {
						sort.Slice(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
					} else {
						sort.Slice(list, func(a, b int) bool { return list[a].Seq > list[b].Seq })
					}
					out := make([]json.RawMessage, 0, len(list))
					for _, i := range list {
						out = append(out, json.RawMessage(r.issueJSONCached(i)))
					}
					return sendPageRaw(rq, out)
				}
				if method == "POST" {
					title := strVal(body["title"])
					var labelObjs []string
					if arr, ok := body["labels"].([]any); ok && len(arr) > 0 {
						if _, isObj := arr[0].(map[string]any); isObj {
							for _, l := range arr {
								if m, ok := l.(map[string]any); ok {
									labelObjs = append(labelObjs, strVal(m["name"]))
								}
							}
						} else {
							for _, l := range arr {
								if s, ok := l.(string); ok {
									labelObjs = append(labelObjs, s)
								}
							}
						}
					}
					made, errStr := createIssueIn(r, title, strVal(body["body"]), labelObjs)
					if errStr != "" {
						return errResp(422, errStr)
					}
					return &resp{status: 201, ctype: jsonType, body: r.issueJSONCached(made)}
				}
			}
			if num, err := strconv.Atoi(rest[1]); err == nil && len(rest) >= 2 {
				iss := r.Issues[num]
				if len(rest) == 2 {
					if iss == nil {
						return errResp(404, "Not Found")
					}
					if method == "GET" {
						return &resp{status: 200, ctype: jsonType, body: r.issueJSONCached(iss)}
					}
					if method == "PATCH" {
						if s, ok := body["state"].(string); ok && (s == "open" || s == "closed") {
							iss.State = s
						}
						if t, ok := body["title"].(string); ok {
							iss.Title = t
						}
						if b, ok := body["body"].(string); ok {
							iss.Body = b
						}
						r.touch()
						return &resp{status: 200, ctype: jsonType, body: r.issueJSONCached(iss)}
					}
				}
				if len(rest) == 3 && rest[2] == "comments" {
					if iss == nil {
						return errResp(404, "Not Found")
					}
					if method == "GET" {
						list := []any{}
						for _, c := range iss.Comments {
							list = append(list, jmap{"id": c.ID, "node_id": gid("IssueComment", strconv.Itoa(r.ID)+":"+strconv.Itoa(num)+":"+strconv.Itoa(c.ID)), "body": c.Body, "user": jmap{"login": viewerLogin, "id": viewerID, "type": "User"}, "created_at": nowISO, "updated_at": nowISO})
						}
						return jsonResp(200, list, nil)
					}
					if method == "POST" {
						id := r.NextComment
						r.NextComment++
						iss.Comments = append(iss.Comments, comment{ID: id, Body: strVal(body["body"])})
						r.touch()
						return jsonResp(201, jmap{"id": id, "node_id": gid("IssueComment", strconv.Itoa(r.ID)+":"+strconv.Itoa(num)+":"+strconv.Itoa(id)), "body": strVal(body["body"]), "user": jmap{"login": viewerLogin, "id": viewerID, "type": "User"}, "created_at": nowISO, "updated_at": nowISO}, nil)
					}
				}
			}
		}
		if rest[0] == "pulls" {
			if len(rest) == 1 {
				if method == "GET" {
					list := []*pull{}
					for _, p := range r.Pulls {
						list = append(list, p)
					}
					state := rq.query.Get("state")
					if state == "" {
						state = "open"
					}
					if state == "open" {
						filtered := list[:0]
						for _, p := range list {
							if p.State == "open" {
								filtered = append(filtered, p)
							}
						}
						list = filtered
					} else if state == "closed" {
						filtered := list[:0]
						for _, p := range list {
							if p.State == "closed" {
								filtered = append(filtered, p)
							}
						}
						list = filtered
					}
					sort.Slice(list, func(a, b int) bool { return list[a].Seq > list[b].Seq })
					out := make([]any, 0, len(list))
					for _, p := range list {
						out = append(out, pullJSON(r, p))
					}
					return sendPage(rq, out)
				}
				if method == "POST" {
					head := strVal(body["head"])
					base := strVal(body["base"], r.DefaultBranch)
					made, errStr := createPullIn(r, strVal(body["title"]), strVal(body["body"]), head, base)
					if errStr != "" {
						return errResp(422, errStr)
					}
					return jsonResp(201, pullJSON(r, made), nil)
				}
			}
			if num, err := strconv.Atoi(rest[1]); err == nil && len(rest) >= 2 {
				pr := r.Pulls[num]
				if pr == nil {
					return errResp(404, "Not Found")
				}
				if len(rest) == 2 && method == "GET" {
					return jsonResp(200, pullJSON(r, pr), nil)
				}
				if len(rest) == 3 && rest[2] == "files" && method == "GET" {
					files := []any{}
					for _, f := range r.prFiles(pr) {
						files = append(files, jmap{"filename": f.Filename, "status": f.Status, "additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes, "sha": "", "patch": ""})
					}
					return jsonResp(200, files, nil)
				}
				if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
					if pr.State != "open" {
						return errResp(405, "Pull request is not open")
					}
					sha, ok := r.mergePR(pr)
					if !ok {
						return errResp(405, "Merge conflict")
					}
					return jsonResp(200, jmap{"sha": sha, "merged": true, "message": "Pull request successfully merged"}, nil)
				}
			}
		}
		return errResp(404, "Not Found")
	}
	return errResp(404, "Not Found")
}

// sendPageRaw: like sendPage but items are pre-marshalled JSON fragments
// (used for the issue list so the cached bytes are reused).
func sendPageRaw(rq *req, items []json.RawMessage) *resp {
	page, perPage := pageParams(rq.query)
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	var extra map[string]string
	if start+perPage < len(items) {
		q := rq.query
		q.Set("page", strconv.Itoa(page+1))
		q.Set("per_page", strconv.Itoa(perPage))
		next := "http://" + rq.host + rq.rawPath + "?" + q.Encode()
		extra = map[string]string{"link": "<" + next + ">; rel=\"next\""}
	}
	var buf strings.Builder
	buf.WriteByte('[')
	for i, it := range items[start:end] {
		if i > 0 {
			buf.WriteByte(',')
		}
		b := []byte(it)
		if len(b) > 0 && b[len(b)-1] == '\n' {
			b = b[:len(b)-1]
		}
		buf.Write(b)
	}
	buf.WriteString("]\n")
	return &resp{status: 200, ctype: jsonType, extra: extra, body: []byte(buf.String())}
}
