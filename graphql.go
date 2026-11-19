// GraphQL v4 (/api/graphql): selection-set parser with variables,
// fragments and aliases, field validation (unknown field = 200 + errors),
// introspection for Issue/PullRequest/Repository + SearchType, and the
// createPullRequest / createIssue / closeIssue / mergePullRequest /
// addComment mutations on the same store as REST.
package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type gqlNode struct {
	kind     string // "field" | "frag"
	name     string
	alias    string
	args     map[string]any
	children []*gqlNode
	cond     string
}

type gval = map[string]any

func gqlTokenize(src string) []string {
	var toks []string
	i := 0
	for i < len(src) {
		c := src[i]
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '.' && i+3 <= len(src) && src[i:i+3] == "..." {
			toks = append(toks, "...")
			i += 3
			continue
		}
		if strings.IndexByte("{}():[]!=$", c) >= 0 {
			toks = append(toks, string(c))
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					out.WriteByte(src[j+1])
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			b, _ := json.Marshal(out.String())
			toks = append(toks, string(b))
			i = j + 1
			continue
		}
		j := i
		for j < len(src) {
			d := src[j]
			if d >= 'A' && d <= 'Z' || d >= 'a' && d <= 'z' || d >= '0' && d <= '9' || d == '_' || d == '.' || d == '-' {
				j++
			} else {
				break
			}
		}
		if j == i {
			i++
			continue
		}
		toks = append(toks, src[i:j])
		i = j
	}
	return toks
}

type gqlParser struct {
	toks []string
	pos  int
	vars map[string]any
}

func (p *gqlParser) parseValue() any {
	t := p.toks[p.pos]
	p.pos++
	switch {
	case t == "$":
		name := p.toks[p.pos]
		p.pos++
		return p.vars[name]
	case t == "{":
		obj := map[string]any{}
		for p.toks[p.pos] != "}" {
			k := p.toks[p.pos]
			p.pos++
			if p.toks[p.pos] == ":" {
				p.pos++
			}
			obj[k] = p.parseValue()
		}
		p.pos++
		return obj
	case t == "[":
		arr := []any{}
		for p.toks[p.pos] != "]" {
			arr = append(arr, p.parseValue())
		}
		p.pos++
		return arr
	case strings.HasPrefix(t, "\""):
		var s string
		_ = json.Unmarshal([]byte(t), &s)
		return s
	case t == "true":
		return true
	case t == "false":
		return false
	case t == "null":
		return nil
	default:
		if n, err := strconv.ParseFloat(t, 64); err == nil && t != "" {
			return n
		}
		return t
	}
}

func (p *gqlParser) parseSelection() []*gqlNode {
	p.pos++ // '{'
	var nodes []*gqlNode
	for p.pos < len(p.toks) && p.toks[p.pos] != "}" {
		if p.toks[p.pos] == "..." {
			p.pos++
			cond := ""
			if p.toks[p.pos] == "on" {
				p.pos++
				cond = p.toks[p.pos]
				p.pos++
			}
			children := p.parseSelection()
			nodes = append(nodes, &gqlNode{kind: "frag", cond: cond, children: children})
			continue
		}
		name := p.toks[p.pos]
		p.pos++
		alias := ""
		if p.toks[p.pos] == ":" {
			alias = name
			p.pos++
			name = p.toks[p.pos]
			p.pos++
		}
		args := map[string]any{}
		if p.toks[p.pos] == "(" {
			p.pos++
			for p.toks[p.pos] != ")" {
				k := p.toks[p.pos]
				p.pos++
				if p.toks[p.pos] == ":" {
					p.pos++
				}
				args[k] = p.parseValue()
			}
			p.pos++
		}
		var children []*gqlNode
		if p.toks[p.pos] == "{" {
			children = p.parseSelection()
		}
		nodes = append(nodes, &gqlNode{kind: "field", name: name, alias: alias, args: args, children: children})
	}
	p.pos++
	return nodes
}

func gqlParseOp(query string, vars map[string]any) (string, []*gqlNode) {
	toks := gqlTokenize(query)
	p := &gqlParser{toks: toks, vars: vars}
	op := "query"
	for p.pos < len(toks) && toks[p.pos] != "{" {
		t := toks[p.pos]
		switch {
		case t == "mutation":
			op = "mutation"
			p.pos++
		case t == "query":
			p.pos++
		case t == "(":
			depth := 0
			for p.pos < len(toks) {
				if toks[p.pos] == "(" {
					depth++
				}
				if toks[p.pos] == ")" {
					depth--
					if depth == 0 {
						p.pos++
						break
					}
				}
				p.pos++
			}
		default:
			p.pos++
		}
	}
	var nodes []*gqlNode
	if p.pos < len(toks) {
		nodes = p.parseSelection()
	}
	return op, nodes
}

var gqlFields = map[string][]string{
	"Query":                            {"viewer", "repository", "node", "__type", "__schema", "__typename"},
	"Node":                             {"id", "__typename"},
	"Mutation":                         {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment", "__typename"},
	"User":                             {"login", "id", "name", "email", "__typename"},
	"Repository":                       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "__typename"},
	"Issue":                            {"id", "number", "title", "state", "body", "createdAt", "author", "labels", "comments", "__typename"},
	"IssueConnection":                  {"nodes", "edges", "pageInfo", "totalCount", "__typename"},
	"PullRequest":                      {"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "body", "author", "__typename"},
	"PullRequestConnection":            {"nodes", "edges", "pageInfo", "totalCount", "__typename"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount", "__typename"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType", "__typename"},
	"Ref":                              {"name", "id", "__typename"},
	"PageInfo":                         {"hasNextPage", "hasPreviousPage", "endCursor", "startCursor", "__typename"},
	"__Type":                           {"name", "kind", "fields", "enumValues", "description", "__typename"},
	"__Field":                          {"name", "description", "__typename"},
	"__EnumValue":                      {"name", "description", "__typename"},
	"__Schema":                         {"types", "queryType", "mutationType", "__typename"},
	"CreateIssuePayload":               {"issue", "__typename"},
	"CloseIssuePayload":                {"issue", "__typename"},
	"CreatePullRequestPayload":         {"pullRequest", "__typename"},
	"MergePullRequestPayload":          {"pullRequest", "__typename"},
	"AddCommentPayload":                {"commentEdge", "__typename"},
	"IssueCommentEdge":                 {"node", "cursor", "__typename"},
	"IssueComment":                     {"id", "body", "__typename"},
	"LabelConnection":                  {"nodes", "__typename"},
	"Label":                            {"name", "color", "__typename"},
}

var gqlTypeOf = map[string]map[string]string{
	"Query":                            {"viewer": "User", "repository": "Repository", "node": "Node", "__type": "__Type", "__schema": "__Schema"},
	"Mutation":                         {"createIssue": "CreateIssuePayload", "closeIssue": "CloseIssuePayload", "createPullRequest": "CreatePullRequestPayload", "mergePullRequest": "MergePullRequestPayload", "addComment": "AddCommentPayload"},
	"Repository":                       {"owner": "User", "defaultBranchRef": "Ref", "issue": "Issue", "issues": "IssueConnection", "pullRequests": "PullRequestConnection", "pullRequest": "PullRequest"},
	"Issue":                            {"author": "User", "labels": "LabelConnection"},
	"IssueConnection":                  {"nodes": "Issue", "pageInfo": "PageInfo"},
	"PullRequest":                      {"files": "PullRequestChangedFileConnection", "author": "User"},
	"PullRequestConnection":            {"nodes": "PullRequest", "pageInfo": "PageInfo"},
	"PullRequestChangedFileConnection": {"nodes": "PullRequestChangedFile", "pageInfo": "PageInfo"},
	"__Type":                           {"fields": "__Field", "enumValues": "__EnumValue"},
	"CreateIssuePayload":               {"issue": "Issue"},
	"CloseIssuePayload":                {"issue": "Issue"},
	"CreatePullRequestPayload":         {"pullRequest": "PullRequest"},
	"MergePullRequestPayload":          {"pullRequest": "PullRequest"},
	"AddCommentPayload":                {"commentEdge": "IssueCommentEdge"},
	"IssueCommentEdge":                 {"node": "IssueComment"},
	"LabelConnection":                  {"nodes": "Label"},
}

var typeFieldsIntro = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "createdAt", "author", "labels", "comments", "url"},
	"PullRequest": {"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "body", "author"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url"},
	"User":        {"login", "id", "name", "email"},
	"SearchType":  {},
}

var searchEnums = []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}

func repoVal(r *repo) gval            { return gval{"__type": "Repository", "repo": r} }
func issueVal(r *repo, i *issue) gval { return gval{"__type": "Issue", "repo": r, "issue": i} }
func pullVal(r *repo, p *pull) gval   { return gval{"__type": "PullRequest", "repo": r, "pr": p} }

func decodeGid(value string) (kind, key string, ok bool) {
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", "", false
	}
	text := string(b)
	i := strings.IndexByte(text, ':')
	if i < 0 {
		return "", "", false
	}
	return text[:i], text[i+1:], true
}

func resolveNodeByGid(value string) gval {
	kind, key, ok := decodeGid(value)
	if !ok {
		return nil
	}
	switch kind {
	case "Repository":
		r := st.findRepoByID(toInt(parseNum(key)))
		if r != nil {
			return repoVal(r)
		}
	case "Issue":
		parts := strings.SplitN(key, ":", 2)
		r := st.findRepoByID(toInt(parseNum(parts[0])))
		if r != nil && len(parts) == 2 {
			if iss := r.Issues[toInt(parseNum(parts[1]))]; iss != nil {
				return issueVal(r, iss)
			}
		}
	case "PullRequest":
		parts := strings.SplitN(key, ":", 2)
		r := st.findRepoByID(toInt(parseNum(parts[0])))
		if r != nil && len(parts) == 2 {
			if pr := r.Pulls[toInt(parseNum(parts[1]))]; pr != nil {
				return pullVal(r, pr)
			}
		}
	}
	return nil
}

func parseNum(s string) float64 {
	n, _ := strconv.ParseFloat(s, 64)
	return n
}

func cursorOf(n int) string { return b64enc("cursor:" + strconv.Itoa(n)) }

func cursorNum(c any) (int, bool) {
	s, ok := c.(string)
	if !ok || s == "" {
		return 0, false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	t := string(b)
	if !strings.HasPrefix(t, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(t[7:])
	if err != nil {
		return 0, false
	}
	return n, true
}

func issueConn(r *repo, args map[string]any) gval {
	list := []*issue{}
	for _, i := range r.Issues {
		list = append(list, i)
	}
	if states, ok := args["states"].([]any); ok {
		want := map[string]bool{}
		for _, s := range states {
			want[strings.ToLower(strVal(s))] = true
		}
		filtered := list[:0]
		for _, i := range list {
			if want[i.State] {
				filtered = append(filtered, i)
			}
		}
		list = filtered
	}
	orderBy, _ := args["orderBy"].(map[string]any)
	dir := strVal(orderBy["direction"], "DESC")
	sort.Slice(list, func(a, b int) bool {
		if dir == "ASC" {
			return list[a].Number < list[b].Number
		}
		return list[a].Number > list[b].Number
	})
	first := 100
	if f, ok := args["first"].(float64); ok {
		first = int(f)
	}
	start := 0
	if after, ok := cursorNum(args["after"]); ok {
		idx := -1
		for i, iss := range list {
			if iss.Number == after {
				idx = i
				break
			}
		}
		if idx >= 0 {
			start = idx + 1
		}
		if dir != "DESC" {
			if _, hasField := orderBy["field"]; !hasField {
				sort.Slice(list, func(a, b int) bool { return list[a].Number < list[b].Number })
				idx2 := -1
				for i, iss := range list {
					if iss.Number == after {
						idx2 = i
						break
					}
				}
				if idx2 >= 0 {
					start = idx2 + 1
				} else {
					start = 0
				}
			}
		}
	}
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	page := list[start:end]
	hasNext := start+first < len(list)
	nodes := make([]any, 0, len(page))
	for _, iss := range page {
		nodes = append(nodes, issueVal(r, iss))
	}
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = cursorOf(page[len(page)-1].Number)
		startCursor = cursorOf(page[0].Number)
	}
	return gval{
		"__type": "IssueConnection", "repo": r, "nodes": nodes,
		"pageInfo":   gval{"hasNextPage": hasNext, "hasPreviousPage": start > 0, "endCursor": endCursor, "startCursor": startCursor},
		"totalCount": len(list),
	}
}

func execField(typeName string, val gval, node *gqlNode) any {
	name := node.name
	if name == "__typename" {
		return typeName
	}
	switch typeName {
	case "Query":
		switch name {
		case "viewer":
			return gval{"__type": "User"}
		case "repository":
			r := st.findRepo(strVal(node.args["owner"]), strVal(node.args["name"]))
			if r == nil {
				return nil
			}
			return repoVal(r)
		case "node":
			return resolveNodeByGid(strVal(node.args["id"]))
		case "__type":
			tname := strVal(node.args["name"])
			if tname == "SearchType" {
				return gval{"__type": "__Type", "tname": tname, "kind": "ENUM"}
			}
			return gval{"__type": "__Type", "tname": tname, "kind": "OBJECT"}
		case "__schema":
			return gval{"__type": "__Schema"}
		}
		return nil
	case "Mutation":
		input, _ := node.args["input"].(map[string]any)
		switch name {
		case "createIssue":
			kind, key, ok := decodeGid(strVal(input["repositoryId"]))
			_ = kind
			var r *repo
			if ok {
				r = st.findRepoByID(toInt(parseNum(key)))
			}
			if r == nil {
				return gval{"__type": "CreateIssuePayload", "issue": nil}
			}
			made, errStr := createIssueIn(r, strVal(input["title"]), strVal(input["body"]), nil)
			if errStr != "" {
				return gval{"__type": "CreateIssuePayload", "issue": nil}
			}
			return gval{"__type": "CreateIssuePayload", "issue": issueVal(r, made)}
		case "closeIssue":
			target := resolveNodeByGid(strVal(input["issueId"]))
			if target != nil && target["__type"] == "Issue" {
				iss := target["issue"].(*issue)
				iss.State = "closed"
				target["repo"].(*repo).touch()
				return gval{"__type": "CloseIssuePayload", "issue": target}
			}
			return gval{"__type": "CloseIssuePayload", "issue": nil}
		case "createPullRequest":
			_, key, ok := decodeGid(strVal(input["repositoryId"]))
			var r *repo
			if ok {
				r = st.findRepoByID(toInt(parseNum(key)))
			}
			if r == nil {
				return gval{"__type": "CreatePullRequestPayload", "pullRequest": nil}
			}
			made, errStr := createPullIn(r, strVal(input["title"]), strVal(input["body"]), strVal(input["headRefName"]), strVal(input["baseRefName"]))
			if errStr != "" {
				return gval{"__type": "CreatePullRequestPayload", "pullRequest": nil}
			}
			return gval{"__type": "CreatePullRequestPayload", "pullRequest": pullVal(r, made)}
		case "mergePullRequest":
			target := resolveNodeByGid(strVal(input["pullRequestId"]))
			if target != nil && target["__type"] == "PullRequest" {
				r := target["repo"].(*repo)
				pr := target["pr"].(*pull)
				if pr.State == "open" {
					r.mergePR(pr)
				}
				return gval{"__type": "MergePullRequestPayload", "pullRequest": target}
			}
			return gval{"__type": "MergePullRequestPayload", "pullRequest": nil}
		case "addComment":
			target := resolveNodeByGid(strVal(input["subjectId"]))
			if target != nil && target["__type"] == "Issue" {
				r := target["repo"].(*repo)
				iss := target["issue"].(*issue)
				id := r.NextComment
				r.NextComment++
				iss.Comments = append(iss.Comments, comment{ID: id, Body: strVal(input["body"])})
				r.touch()
				return gval{"__type": "AddCommentPayload", "commentEdge": gval{"__type": "IssueCommentEdge", "node": gval{"__type": "IssueComment", "id": gid("IssueComment", strconv.Itoa(r.ID)+":"+strconv.Itoa(iss.Number)+":"+strconv.Itoa(id)), "body": strVal(input["body"])}}}
			}
			return gval{"__type": "AddCommentPayload", "commentEdge": nil}
		}
		return nil
	case "User":
		switch name {
		case "login":
			return viewerLogin
		case "id":
			return gid("User", viewerLogin)
		case "name":
			return viewerName
		case "email":
			return viewerEmail
		}
		return nil
	case "Repository":
		r := val["repo"].(*repo)
		switch name {
		case "id":
			return gid("Repository", strconv.Itoa(r.ID))
		case "databaseId":
			return r.ID
		case "name":
			return r.Name
		case "nameWithOwner":
			return r.Owner + "/" + r.Name
		case "owner":
			return gval{"__type": "User"}
		case "defaultBranchRef":
			if r.defaultTip() == "" {
				return nil
			}
			return gval{"__type": "Ref", "name": r.DefaultBranch}
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "issue":
			iss := r.Issues[toInt(node.args["number"])]
			if iss == nil {
				return nil
			}
			return issueVal(r, iss)
		case "issues":
			return issueConn(r, node.args)
		case "pullRequests":
			list := []*pull{}
			for _, p := range r.Pulls {
				if p.State == "open" {
					list = append(list, p)
				}
			}
			sort.Slice(list, func(a, b int) bool { return list[a].Number > list[b].Number })
			first := 100
			if f, ok := node.args["first"].(float64); ok {
				first = int(f)
			}
			end := first
			if end > len(list) {
				end = len(list)
			}
			nodes := make([]any, 0, end)
			for _, p := range list[:end] {
				nodes = append(nodes, pullVal(r, p))
			}
			return gval{"__type": "PullRequestConnection", "repo": r, "nodes": nodes,
				"pageInfo":   gval{"hasNextPage": len(list) > first, "hasPreviousPage": false, "endCursor": nil, "startCursor": nil},
				"totalCount": len(list)}
		case "pullRequest":
			pr := r.Pulls[toInt(node.args["number"])]
			if pr == nil {
				return nil
			}
			return pullVal(r, pr)
		}
		return nil
	case "Issue":
		r := val["repo"].(*repo)
		iss := val["issue"].(*issue)
		switch name {
		case "id":
			return gid("Issue", strconv.Itoa(r.ID)+":"+strconv.Itoa(iss.Number))
		case "number":
			return iss.Number
		case "title":
			return iss.Title
		case "state":
			return strings.ToUpper(iss.State)
		case "body":
			return iss.Body
		case "createdAt":
			return nowISO
		case "author":
			return gval{"__type": "User"}
		case "labels":
			nodes := make([]any, 0, len(iss.Labels))
			for _, l := range iss.Labels {
				nodes = append(nodes, gval{"__type": "Label", "name": l.Name, "color": l.Color})
			}
			return gval{"__type": "LabelConnection", "nodes": nodes}
		}
		return nil
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		switch name {
		case "nodes":
			return val["nodes"]
		case "pageInfo":
			return val["pageInfo"]
		case "totalCount":
			return val["totalCount"]
		case "edges":
			nodes, _ := val["nodes"].([]any)
			edges := make([]any, 0, len(nodes))
			for _, n := range nodes {
				edges = append(edges, gval{"cursor": "", "node": n})
			}
			return edges
		}
		return nil
	case "PullRequest":
		r := val["repo"].(*repo)
		pr := val["pr"].(*pull)
		switch name {
		case "id":
			return gid("PullRequest", strconv.Itoa(r.ID)+":"+strconv.Itoa(pr.Number))
		case "number":
			return pr.Number
		case "title":
			return pr.Title
		case "state":
			if pr.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pr.State)
		case "merged":
			return pr.Merged
		case "headRefName":
			return pr.Head
		case "baseRefName":
			return pr.Base
		case "mergeable":
			if r.prConflicted(pr) {
				return "CONFLICTING"
			}
			return "MERGEABLE"
		case "body":
			return pr.Body
		case "author":
			return gval{"__type": "User"}
		case "files":
			files := r.prFiles(pr)
			nodes := make([]any, 0, len(files))
			for _, f := range files {
				nodes = append(nodes, gval{"__type": "PullRequestChangedFile", "path": f.Filename, "additions": f.Additions, "deletions": f.Deletions, "changeType": strings.ToUpper(f.Status)})
			}
			first := 100
			if f, ok := node.args["first"].(float64); ok {
				first = int(f)
			}
			if first < len(nodes) {
				nodes = nodes[:first]
			}
			return gval{"__type": "PullRequestChangedFileConnection", "nodes": nodes,
				"pageInfo":   gval{"hasNextPage": false, "hasPreviousPage": false, "endCursor": nil, "startCursor": nil},
				"totalCount": len(files)}
		}
		return nil
	case "PullRequestChangedFile":
		if v, ok := val[name]; ok {
			return v
		}
		switch name {
		case "path":
			return val["path"]
		case "additions":
			return val["additions"]
		case "deletions":
			return val["deletions"]
		case "changeType":
			return val["changeType"]
		}
		return nil
	case "Ref":
		if name == "name" {
			return val["name"]
		}
		if name == "id" {
			return gid("Ref", strVal(val["name"]))
		}
		return nil
	case "PageInfo":
		return val[name]
	case "__Type":
		tname := strVal(val["tname"])
		kind := strVal(val["kind"])
		switch name {
		case "name":
			return tname
		case "kind":
			return kind
		case "fields":
			if kind == "ENUM" {
				return nil
			}
			names := typeFieldsIntro[tname]
			if names == nil {
				names = gqlFields[tname]
			}
			out := make([]any, 0, len(names))
			for _, n := range names {
				out = append(out, gval{"__type": "__Field", "name": n})
			}
			return out
		case "enumValues":
			if tname == "SearchType" {
				out := make([]any, 0, len(searchEnums))
				for _, n := range searchEnums {
					out = append(out, gval{"__type": "__EnumValue", "name": n})
				}
				return out
			}
			return nil
		case "description":
			return nil
		}
		return nil
	case "__Field", "__EnumValue":
		if name == "name" {
			return val["name"]
		}
		if name == "description" {
			return nil
		}
		return nil
	case "__Schema":
		return nil
	case "CreateIssuePayload", "CloseIssuePayload":
		if name == "issue" {
			return val["issue"]
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if name == "pullRequest" {
			return val["pullRequest"]
		}
		return nil
	case "AddCommentPayload":
		if name == "commentEdge" {
			return val["commentEdge"]
		}
		return nil
	case "IssueCommentEdge":
		if name == "node" {
			return val["node"]
		}
		if name == "cursor" {
			return ""
		}
		return nil
	case "IssueComment":
		return val[name]
	case "LabelConnection":
		if name == "nodes" {
			return val["nodes"]
		}
		return nil
	case "Label":
		return val[name]
	case "Node":
		if name == "id" {
			if val["__type"] == "Repository" {
				return gid("Repository", strconv.Itoa(val["repo"].(*repo).ID))
			}
			return nil
		}
		return nil
	}
	return nil
}

func childType(parent, field string) string {
	if parent == "Node" {
		return "Node"
	}
	if m, ok := gqlTypeOf[parent]; ok {
		if ct, ok := m[field]; ok {
			return ct
		}
	}
	return "String"
}

func validateNodes(nodes []*gqlNode, typeName string, errors *[]string) {
	for _, node := range nodes {
		if node.kind == "frag" {
			cond := node.cond
			if cond == "" {
				cond = typeName
			}
			validateNodes(node.children, cond, errors)
			continue
		}
		allowed := gqlFields[typeName]
		found := false
		for _, a := range allowed {
			if a == node.name {
				found = true
				break
			}
		}
		if !found {
			*errors = append(*errors, "Cannot query field \""+node.name+"\" on type \""+typeName+"\".")
			continue
		}
		if len(node.children) > 0 {
			ct := childType(typeName, node.name)
			if ct != "String" && ct != "Node" {
				validateNodes(node.children, ct, errors)
			} else if ct == "Node" {
				validateNodes(node.children, "Node", errors)
			}
		}
	}
}

func execNodes(nodes []*gqlNode, typeName string, val gval) map[string]any {
	out := map[string]any{}
	for _, node := range nodes {
		if node.kind == "frag" {
			if node.cond == "" || node.cond == typeName || (typeName == "Node" && val != nil) {
				cond := node.cond
				if cond == "" {
					cond = typeName
				}
				for k, v := range execNodes(node.children, cond, val) {
					out[k] = v
				}
			}
			continue
		}
		key := node.name
		if node.alias != "" {
			key = node.alias
		}
		if val == nil {
			out[key] = nil
			continue
		}
		result := execField(typeName, val, node)
		if result != nil && len(node.children) > 0 {
			ct := childType(typeName, node.name)
			switch res := result.(type) {
			case []any:
				arr := make([]any, 0, len(res))
				for _, item := range res {
					if m, ok := item.(gval); ok {
						// v1 recurses only into items carrying __type;
						// plain items (e.g. edges) are returned as-is.
						if t := strVal(m["__type"]); t != "" {
							arr = append(arr, execNodes(node.children, t, m))
						} else {
							arr = append(arr, item)
						}
					} else {
						arr = append(arr, item)
					}
				}
				out[key] = arr
			case gval:
				t := strVal(res["__type"])
				if t == "" {
					// Plain object result (e.g. PageInfo): the child
					// type from the schema decides.
					t = ct
				}
				out[key] = execNodes(node.children, t, res)
			default:
				out[key] = result
			}
		} else {
			out[key] = result
		}
	}
	return out
}

func runGraphQLOp(query string, variables map[string]any) map[string]any {
	op, nodes := gqlParseOp(query, variables)
	var errors []string
	rootType := "Query"
	if op == "mutation" {
		rootType = "Mutation"
	}
	validateNodes(nodes, rootType, &errors)
	if len(errors) > 0 {
		data := map[string]any{}
		for _, n := range nodes {
			if n.kind == "field" {
				k := n.name
				if n.alias != "" {
					k = n.alias
				}
				data[k] = nil
			}
		}
		errs := make([]any, 0, len(errors))
		for _, m := range errors {
			errs = append(errs, map[string]any{"message": m})
		}
		return map[string]any{"data": data, "errors": errs}
	}
	root := gval{"__type": rootType}
	return map[string]any{"data": execNodes(nodes, rootType, root)}
}

func handleGraphQL(rq *req) *resp {
	if !authed(rq) {
		return errResp(401, "Requires authentication")
	}
	var raw any
	if err := json.Unmarshal(rq.body, &raw); err != nil {
		return errResp(400, "Invalid JSON")
	}
	if arr, isArr := raw.([]any); isArr {
		out := make([]any, 0, len(arr))
		for _, entry := range arr {
			m, ok := entry.(map[string]any)
			if !ok {
				out = append(out, map[string]any{"data": nil, "errors": []any{map[string]any{"message": "invalid operation"}}})
				continue
			}
			vars, _ := m["variables"].(map[string]any)
			out = append(out, runGraphQLOp(strVal(m["query"]), vars))
		}
		return jsonResp(200, out, nil)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return errResp(400, "Invalid GraphQL body")
	}
	vars, _ := m["variables"].(map[string]any)
	return jsonResp(200, runGraphQLOp(strVal(m["query"]), vars), nil)
}
