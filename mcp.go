// MCP (/mcp): JSON-RPC 2.0 Streamable HTTP. initialize negotiates the
// protocol version and returns Mcp-Session-Id; server/discover advertises
// 2026-07-28 / 2025-11-25; tools/list is byte-identical across calls;
// notifications/* answer 202 empty; unknown tool -32602, unknown method
// -32601, non-JSON body 400 -32700, unauthenticated 401 -32001.
package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

type mcpProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema mcpInputSchema `json:"inputSchema"`
}

type mcpInputSchema struct {
	Type       string             `json:"type"`
	Properties map[string]mcpProp `json:"properties"`
	Required   []string           `json:"required"`
}

var mcpTools = []mcpToolDef{
	{Name: "create_repo", Description: "Create a repository owned by the authenticated user.", InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
		"name":        {Type: "string", Description: "Repository name"},
		"description": {Type: "string", Description: "Repository description"},
		"private":     {Type: "boolean", Description: "Whether the repository is private"},
	}, Required: []string{"name"}}},
	{Name: "get_repo", Description: "Get a repository by owner and name.", InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
		"owner": {Type: "string", Description: "Repository owner"},
		"repo":  {Type: "string", Description: "Repository name"},
	}, Required: []string{"owner", "repo"}}},
	{Name: "create_issue", Description: "Create an issue in a repository.", InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
		"owner": {Type: "string", Description: "Repository owner"},
		"repo":  {Type: "string", Description: "Repository name"},
		"title": {Type: "string", Description: "Issue title"},
		"body":  {Type: "string", Description: "Issue body"},
	}, Required: []string{"owner", "repo", "title"}}},
	{Name: "list_issues", Description: "List issues in a repository.", InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
		"owner": {Type: "string", Description: "Repository owner"},
		"repo":  {Type: "string", Description: "Repository name"},
		"state": {Type: "string", Description: "Issue state: open, closed or all"},
	}, Required: []string{"owner", "repo"}}},
	{Name: "create_pull_request", Description: "Create a pull request from a head branch into a base branch.", InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
		"owner": {Type: "string", Description: "Repository owner"},
		"repo":  {Type: "string", Description: "Repository name"},
		"title": {Type: "string", Description: "Pull request title"},
		"body":  {Type: "string", Description: "Pull request body"},
		"head":  {Type: "string", Description: "Head branch name"},
		"base":  {Type: "string", Description: "Base branch name"},
	}, Required: []string{"owner", "repo", "title", "head", "base"}}},
	{Name: "merge_pull_request", Description: "Merge a pull request with a squash commit.", InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
		"owner":       {Type: "string", Description: "Repository owner"},
		"repo":        {Type: "string", Description: "Repository name"},
		"pull_number": {Type: "integer", Description: "Pull request number"},
	}, Required: []string{"owner", "repo", "pull_number"}}},
	{Name: "close_issue", Description: "Close an issue.", InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{
		"owner":        {Type: "string", Description: "Repository owner"},
		"repo":         {Type: "string", Description: "Repository name"},
		"issue_number": {Type: "integer", Description: "Issue number"},
	}, Required: []string{"owner", "repo", "issue_number"}}},
	{Name: "list_repos", Description: "List repositories owned by the authenticated user.", InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{}, Required: []string{}}},
}

func toolTextResult(value any, isError bool) jmap {
	var text string
	if s, ok := value.(string); ok {
		text = s
	} else {
		b, _ := json.Marshal(value)
		text = string(b)
	}
	return jmap{
		"content":           []any{jmap{"type": "text", "text": text}},
		"structuredContent": value,
		"isError":           isError,
	}
}

func splitFullName(args jmap) (string, string) {
	owner := strVal(args["owner"])
	repo := strVal(args["repo"])
	full := strVal(args["full_name"])
	if (owner == "" || repo == "") && strings.Contains(full, "/") {
		parts := strings.SplitN(full, "/", 2)
		if owner == "" {
			owner = parts[0]
		}
		if repo == "" {
			repo = parts[1]
		}
	}
	return owner, repo
}

type mcpOutcome struct {
	result jmap
	err    *jmap
}

func callMcpTool(name string, args jmap) mcpOutcome {
	var def *mcpToolDef
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			def = &mcpTools[i]
			break
		}
	}
	if def == nil {
		e := jmap{"code": -32602, "message": "Unknown tool: " + name}
		return mcpOutcome{err: &e}
	}
	missing := []string{}
	for _, req := range def.InputSchema.Required {
		v, ok := args[req]
		if !ok || v == nil || v == "" {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return mcpOutcome{result: toolTextResult("Missing required argument: "+strings.Join(missing, ", "), true)}
	}
	owner, repoName := splitFullName(args)
	if name == "create_repo" {
		rname := strVal(args["name"])
		if _, exists := st.repos[repoKey(viewerLogin, rname)]; exists {
			return mcpOutcome{result: toolTextResult("Repository already exists: "+rname, true)}
		}
		priv, _ := args["private"].(bool)
		r := st.createRepo(rname, priv, strVal(args["description"]))
		return mcpOutcome{result: toolTextResult(repoJSON(r), false)}
	}
	if name == "list_repos" {
		list := []any{}
		for _, r := range st.repos {
			if r.Owner == viewerLogin {
				list = append(list, repoJSON(r))
			}
		}
		return mcpOutcome{result: toolTextResult(list, false)}
	}
	r := st.findRepo(owner, repoName)
	if r == nil {
		return mcpOutcome{result: toolTextResult("Repository not found: "+owner+"/"+repoName, true)}
	}
	switch name {
	case "get_repo":
		return mcpOutcome{result: toolTextResult(repoJSON(r), false)}
	case "create_issue":
		made, errStr := createIssueIn(r, strVal(args["title"]), strVal(args["body"]), nil)
		if errStr != "" {
			return mcpOutcome{result: toolTextResult(errStr, true)}
		}
		return mcpOutcome{result: toolTextResult(issueJSONObj(r, made), false)}
	case "list_issues":
		list := []*issue{}
		for _, i := range r.Issues {
			list = append(list, i)
		}
		state := strVal(args["state"], "open")
		if state != "all" {
			filtered := list[:0]
			for _, i := range list {
				if i.State == state {
					filtered = append(filtered, i)
				}
			}
			list = filtered
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Seq > list[b].Seq })
		out := make([]any, 0, len(list))
		for _, i := range list {
			out = append(out, issueJSONObj(r, i))
		}
		return mcpOutcome{result: toolTextResult(out, false)}
	case "create_pull_request":
		made, errStr := createPullIn(r, strVal(args["title"]), strVal(args["body"]), strVal(args["head"]), strVal(args["base"]))
		if errStr != "" {
			return mcpOutcome{result: toolTextResult(errStr, true)}
		}
		return mcpOutcome{result: toolTextResult(pullJSON(r, made), false)}
	case "merge_pull_request":
		num := toInt(args["pull_number"])
		pr := r.Pulls[num]
		if pr == nil {
			return mcpOutcome{result: toolTextResult("Pull request not found: "+strconv.Itoa(num), true)}
		}
		if pr.State != "open" {
			return mcpOutcome{result: toolTextResult("Pull request is not open", true)}
		}
		sha, ok := r.mergePR(pr)
		if !ok {
			return mcpOutcome{result: toolTextResult("Merge conflict", true)}
		}
		return mcpOutcome{result: toolTextResult(jmap{"merged": true, "sha": sha, "number": pr.Number}, false)}
	case "close_issue":
		num := toInt(args["issue_number"])
		iss := r.Issues[num]
		if iss == nil {
			return mcpOutcome{result: toolTextResult("Issue not found: "+strconv.Itoa(num), true)}
		}
		iss.State = "closed"
		r.touch()
		return mcpOutcome{result: toolTextResult(issueJSONObj(r, iss), false)}
	}
	e := jmap{"code": -32602, "message": "Unknown tool: " + name}
	return mcpOutcome{err: &e}
}

func handleMCP(rq *req) *resp {
	if !authed(rq) {
		body := jbytes(jmap{"jsonrpc": "2.0", "id": nil, "error": jmap{"code": -32001, "message": "Unauthorized"}})
		return &resp{status: 401, ctype: jsonType, body: body}
	}
	var raw any
	if err := json.Unmarshal(rq.body, &raw); err != nil {
		return jsonResp(400, jmap{"jsonrpc": "2.0", "id": nil, "error": jmap{"code": -32700, "message": "Parse error"}}, nil)
	}
	value, isObj := raw.(map[string]any)
	if !isObj {
		return jsonResp(200, jmap{"jsonrpc": "2.0", "id": nil, "error": jmap{"code": -32600, "message": "Invalid Request"}}, nil)
	}
	method, ok := value["method"].(string)
	if !ok {
		id := value["id"]
		if id == nil {
			id = nil
		}
		return jsonResp(200, jmap{"jsonrpc": "2.0", "id": id, "error": jmap{"code": -32600, "message": "Invalid Request"}}, nil)
	}
	id, hasID := value["id"]
	if !hasID {
		id = nil
	}
	params, _ := value["params"].(map[string]any)
	if method == "notifications/initialized" || strings.HasPrefix(method, "notifications/") {
		return &resp{status: 202, body: []byte{}}
	}
	if method == "server/discover" {
		serverInfo := jmap{"name": "github-core-clone", "version": "1.0.0"}
		return jsonResp(200, jmap{
			"jsonrpc": "2.0", "id": id,
			"result": jmap{
				"resultType":        "discover",
				"supportedVersions": []any{"2026-07-28", "2025-11-25"},
				"capabilities":      jmap{"tools": jmap{}},
				"serverInfo":        serverInfo,
				"instructions":      "A GitHub-compatible forge over MCP.",
				"_meta":             jmap{"io.modelcontextprotocol/serverInfo": serverInfo},
			},
		}, nil)
	}
	if method == "initialize" {
		requested := strVal(params["protocolVersion"], "2026-07-28")
		negotiated := "2026-07-28"
		for _, v := range []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"} {
			if requested == v {
				negotiated = requested
				break
			}
		}
		return jsonResp(200, jmap{
			"jsonrpc": "2.0", "id": id,
			"result": jmap{
				"protocolVersion": negotiated,
				"capabilities":    jmap{"tools": jmap{}},
				"serverInfo":      jmap{"name": "github-core-clone", "version": "1.0.0"},
			},
		}, map[string]string{"mcp-session-id": "forge-session-1"})
	}
	if method == "ping" {
		return jsonResp(200, jmap{"jsonrpc": "2.0", "id": id, "result": jmap{}}, nil)
	}
	if method == "tools/list" {
		return jsonResp(200, jmap{"jsonrpc": "2.0", "id": id, "result": jmap{"tools": mcpTools}}, nil)
	}
	if method == "tools/call" {
		toolName := strVal(params["name"])
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = jmap{}
		}
		outcome := callMcpTool(toolName, args)
		if outcome.err != nil {
			return jsonResp(200, jmap{"jsonrpc": "2.0", "id": id, "error": *outcome.err}, nil)
		}
		return jsonResp(200, jmap{"jsonrpc": "2.0", "id": id, "result": outcome.result}, nil)
	}
	return jsonResp(200, jmap{"jsonrpc": "2.0", "id": id, "error": jmap{"code": -32601, "message": "Method not found: " + method}}, nil)
}
