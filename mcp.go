// MCP (Model Context Protocol, streamable HTTP on POST /mcp): the modern
// server/discover era first, with the legacy initialize handshake as a
// fallback. Plain JSON-RPC responses (no SSE needed — the client accepts
// either). Every tool result carries both a text block (JSON) and
// structuredContent; effects are the same store objects REST serves.

package main

import (
	"encoding/json"
	"math"
)

const mcpServerName = "github-forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// mcpToolsJSON is the one constant tool list: tools/list must be byte-identical
// across calls, so it is built once at startup.
var mcpToolsJSON []byte

func init() {
	mcpToolsJSON = buildMCPToolsJSON()
}

// mcpToolProp describes one inputSchema property.
type mcpToolProp struct {
	name        string
	typ         string // "string", "boolean", "integer"
	description string
}

type mcpToolDef struct {
	name        string
	description string
	props       []mcpToolProp
	required    []string
}

var mcpTools = []mcpToolDef{
	{
		name: "create_repo", description: "Create a new repository owned by the authenticated user.",
		props: []mcpToolProp{
			{"owner", "string", "Owner login (defaults to the authenticated user)"},
			{"name", "string", "Repository name"},
			{"description", "string", "Repository description"},
			{"private", "boolean", "Whether the repository is private"},
		},
		required: []string{"name"},
	},
	{
		name: "get_repo", description: "Read one repository by owner and name.",
		props: []mcpToolProp{
			{"owner", "string", "Repository owner login"},
			{"repo", "string", "Repository name"},
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_issue", description: "Create an issue in a repository.",
		props: []mcpToolProp{
			{"owner", "string", "Repository owner login"},
			{"repo", "string", "Repository name"},
			{"title", "string", "Issue title"},
			{"body", "string", "Issue body"},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name: "list_issues", description: "List the issues of a repository.",
		props: []mcpToolProp{
			{"owner", "string", "Repository owner login"},
			{"repo", "string", "Repository name"},
			{"state", "string", "Issue state: open, closed or all"},
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_pull_request", description: "Open a pull request from a head branch into a base branch.",
		props: []mcpToolProp{
			{"owner", "string", "Repository owner login"},
			{"repo", "string", "Repository name"},
			{"title", "string", "Pull request title"},
			{"body", "string", "Pull request body"},
			{"head", "string", "Head branch name"},
			{"base", "string", "Base branch name"},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name: "merge_pull_request", description: "Merge a pull request by number.",
		props: []mcpToolProp{
			{"owner", "string", "Repository owner login"},
			{"repo", "string", "Repository name"},
			{"pull_number", "integer", "Pull request number"},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
	{
		name: "list_repos", description: "List the repositories of the authenticated user.",
		props: nil,
		required: []string{},
	},
	{
		name: "get_issue", description: "Read one issue by owner, repository and number.",
		props: []mcpToolProp{
			{"owner", "string", "Repository owner login"},
			{"repo", "string", "Repository name"},
			{"issue_number", "integer", "Issue number"},
		},
		required: []string{"owner", "repo", "issue_number"},
	},
}

func buildMCPToolsJSON() []byte {
	var w jw
	w.raw(`[`)
	for i, t := range mcpTools {
		if i > 0 {
			w.raw(`,`)
		}
		w.raw(`{"name":`)
		w.str(t.name)
		w.raw(`,"description":`)
		w.str(t.description)
		w.raw(`,"inputSchema":{"type":"object","properties":{`)
		for j, p := range t.props {
			if j > 0 {
				w.raw(`,`)
			}
			w.str(p.name)
			w.raw(`:{"type":`)
			w.str(p.typ)
			w.raw(`,"description":`)
			w.str(p.description)
			w.raw(`}`)
		}
		w.raw(`},"required":[`)
		for j, r := range t.required {
			if j > 0 {
				w.raw(`,`)
			}
			w.str(r)
		}
		w.raw(`]}}`)
	}
	w.raw(`]`)
	return w.bytes()
}

// ---------------------------------------------------------------- tool calls

type mcpOutcome struct {
	// kind: "ok" (result JSON bytes), "toolError" (message), "rpcError" (code+message)
	kind    string
	result  []byte
	message string
	code    int
}

func mcpCallTool(s *Store, name string, args map[string]any) mcpOutcome {
	var def *mcpToolDef
	for i := range mcpTools {
		if mcpTools[i].name == name {
			def = &mcpTools[i]
			break
		}
	}
	if def == nil {
		return mcpOutcome{kind: "rpcError", code: -32602, message: "Unknown tool: " + name}
	}
	for _, required := range def.required {
		v, present := args[required]
		if !present || v == nil {
			return mcpOutcome{kind: "rpcError", code: -32602, message: "Missing required argument: " + required}
		}
		if str, ok := v.(string); ok && str == "" {
			return mcpOutcome{kind: "rpcError", code: -32602, message: "Missing required argument: " + required}
		}
	}
	strArg := func(key string) string {
		if v, ok := args[key].(string); ok {
			return v
		}
		return ""
	}
	intArg := func(key string) (int, bool) {
		v, ok := args[key].(float64)
		if !ok {
			return 0, false
		}
		if v != math.Trunc(v) || v < -9007199254740991 || v > 9007199254740991 {
			return 0, false
		}
		return int(v), true
	}
	repoOf := func() (*repo, bool) {
		owner := strArg("owner")
		if owner == "" {
			owner = viewer.login
		}
		repoName := strArg("repo")
		if repoName == "" {
			return nil, false
		}
		r := s.getRepo(owner, repoName)
		if r == nil {
			return nil, false
		}
		return r, true
	}

	switch name {
	case "create_repo":
		repoName := strArg("name")
		if s.getRepo(viewer.login, repoName) != nil {
			return mcpOutcome{kind: "toolError", message: "Repository " + viewer.login + "/" + repoName + " already exists"}
		}
		isPrivate := false
		if b, ok := args["private"].(bool); ok && b {
			isPrivate = true
		}
		r := s.createRepo(viewer.login, repoName, strArg("description"), isPrivate)
		return mcpOutcome{kind: "ok", result: repoJSON(r)}

	case "list_repos":
		var w jw
		w.raw(`{"repositories":[`)
		repos := s.listRepos(viewer.login)
		for i, r := range repos {
			if i > 0 {
				w.raw(`,`)
			}
			w.buf = append(w.buf, repoJSON(r)...)
		}
		w.raw(`]}`)
		return mcpOutcome{kind: "ok", result: w.bytes()}

	case "get_repo":
		r, ok := repoOf()
		if !ok {
			return mcpOutcome{kind: "toolError", message: "Repository not found"}
		}
		return mcpOutcome{kind: "ok", result: repoJSON(r)}

	case "create_issue":
		r, ok := repoOf()
		if !ok {
			return mcpOutcome{kind: "toolError", message: "Repository not found"}
		}
		iss := s.createIssue(r, strArg("title"), strArg("body"), nil)
		return mcpOutcome{kind: "ok", result: issueJSONBytes(r, iss, nil)}

	case "get_issue":
		r, ok := repoOf()
		number, numOK := intArg("issue_number")
		if !ok || !numOK {
			return mcpOutcome{kind: "toolError", message: "Repository or issue not found"}
		}
		iss := s.findIssue(r, number)
		if iss == nil {
			return mcpOutcome{kind: "toolError", message: "Issue #" + itoa(number) + " not found"}
		}
		return mcpOutcome{kind: "ok", result: issueJSONBytes(r, iss, nil)}

	case "list_issues":
		r, ok := repoOf()
		if !ok {
			return mcpOutcome{kind: "toolError", message: "Repository not found"}
		}
		state := strArg("state")
		if state == "" {
			state = "open"
		}
		var w jw
		w.raw(`{"issues":[`)
		first := true
		for _, iss := range r.issues {
			if state != "all" && iss.state != state {
				continue
			}
			if !first {
				w.raw(`,`)
			}
			first = false
			w.buf = append(w.buf, issueJSONBytes(r, iss, nil)...)
		}
		w.raw(`]}`)
		return mcpOutcome{kind: "ok", result: w.bytes()}

	case "create_pull_request":
		r, ok := repoOf()
		if !ok {
			return mcpOutcome{kind: "toolError", message: "Repository not found"}
		}
		head := strArg("head")
		base := strArg("base")
		if _, ok := s.resolveRef(r, head); !ok {
			return mcpOutcome{kind: "toolError", message: "Head branch '" + head + "' not found"}
		}
		if _, ok := s.resolveRef(r, base); !ok {
			return mcpOutcome{kind: "toolError", message: "Base branch '" + base + "' not found"}
		}
		var p *pull
		for _, existing := range r.pulls {
			if existing.state == "open" && existing.headRef == head && existing.baseRef == base {
				p = existing
				break
			}
		}
		if p == nil {
			p = s.createPull(r, strArg("title"), strArg("body"), head, base)
		}
		if p == nil {
			return mcpOutcome{kind: "toolError", message: "could not create the pull request"}
		}
		return mcpOutcome{kind: "ok", result: pullJSON(s, r, p)}

	case "merge_pull_request":
		r, ok := repoOf()
		number, numOK := intArg("pull_number")
		if !ok || !numOK {
			return mcpOutcome{kind: "toolError", message: "Repository or pull request not found"}
		}
		p := s.findPull(r, number)
		if p == nil {
			return mcpOutcome{kind: "toolError", message: "Pull request #" + itoa(number) + " not found"}
		}
		if p.state != "open" || p.merged {
			return mcpOutcome{kind: "toolError", message: "Pull request #" + itoa(number) + " is not open"}
		}
		if s.pullConflicts(r, p) {
			return mcpOutcome{kind: "toolError", message: "Merge conflict"}
		}
		merged := s.mergePull(r, p)
		if merged == "" {
			return mcpOutcome{kind: "toolError", message: "Merge conflict"}
		}
		var w jw
		w.raw(`{"number":`)
		w.integer(p.number)
		w.raw(`,"merged":true,"state":"closed","merge_commit_sha":`)
		w.str(merged)
		w.raw(`}`)
		return mcpOutcome{kind: "ok", result: w.bytes()}
	}
	return mcpOutcome{kind: "rpcError", code: -32602, message: "Unknown tool: " + name}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------- HTTP entry

func rpcIDJSON(id any) []byte {
	// JSON-RPC id: a string, a number, or null (missing id handled earlier).
	switch v := id.(type) {
	case nil:
		return []byte("null")
	case string:
		b, _ := json.Marshal(v)
		return b
	case float64:
		b, _ := json.Marshal(v)
		return b
	case bool:
		if v {
			return []byte("true")
		}
		return []byte("false")
	default:
		return []byte("null")
	}
}

func mcpResult(id any, result []byte) response {
	var w jw
	w.raw(`{"jsonrpc":"2.0","id":`)
	w.buf = append(w.buf, rpcIDJSON(id)...)
	w.raw(`,"result":`)
	w.buf = append(w.buf, result...)
	w.raw(`}`)
	return jsonBytes(200, w.bytes())
}

func mcpError(status int, id any, code int, message string) response {
	var w jw
	w.raw(`{"jsonrpc":"2.0","id":`)
	w.buf = append(w.buf, rpcIDJSON(id)...)
	w.raw(`,"error":{"code":`)
	w.integer(code)
	w.raw(`,"message":`)
	w.str(message)
	w.raw(`}}`)
	return jsonBytes(status, w.bytes())
}

func mcpIsMutatingTool(name string) bool {
	switch name {
	case "create_repo", "create_issue", "create_pull_request", "merge_pull_request":
		return true
	}
	return false
}

// handleMcp routes one JSON-RPC request on POST /mcp.
func handleMcp(s *Store, req *request) response {
	var parsed any
	if err := json.Unmarshal(req.body, &parsed); err != nil {
		return mcpError(400, nil, -32700, "Parse error")
	}
	rec, isRec := parsed.(map[string]any)
	if !isRec {
		return mcpError(400, nil, -32600, "Invalid Request")
	}
	method, isMethod := rec["method"].(string)
	if !isMethod {
		var id any
		if v, ok := rec["id"]; ok {
			id = v
		}
		return mcpError(400, id, -32600, "Invalid Request")
	}
	id, hasID := rec["id"]
	if !hasID {
		id = nil
	}
	params, _ := rec["params"].(map[string]any)

	// A notification (no id): acknowledge with an empty 200 and do nothing.
	if _, present := rec["id"]; !present {
		return emptyResp(200)
	}

	switch method {
	case "server/discover":
		var w jw
		w.raw(`{"supportedVersions":`)
		w.buf = append(w.buf, jsonStringArray(mcpSupportedVersions)...)
		w.raw(`,"capabilities":{"tools":{}},"serverInfo":{"name":`)
		w.str(mcpServerName)
		w.raw(`,"version":`)
		w.str(mcpServerVersion)
		w.raw(`},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":`)
		w.str(mcpServerName)
		w.raw(`,"version":`)
		w.str(mcpServerVersion)
		w.raw(`}}}`)
		return mcpResult(id, w.bytes())

	case "initialize":
		offered, _ := params["protocolVersion"].(string)
		negotiated := "2026-07-28"
		for _, v := range mcpSupportedVersions {
			if v == offered {
				negotiated = v
				break
			}
		}
		var w jw
		w.raw(`{"protocolVersion":`)
		w.str(negotiated)
		w.raw(`,"capabilities":{"tools":{}},"serverInfo":{"name":`)
		w.str(mcpServerName)
		w.raw(`,"version":`)
		w.str(mcpServerVersion)
		w.raw(`}}`)
		return mcpResult(id, w.bytes())

	case "ping":
		return mcpResult(id, []byte(`{}`))

	case "tools/list":
		var w jw
		w.raw(`{"tools":`)
		w.buf = append(w.buf, mcpToolsJSON...)
		w.raw(`}`)
		return mcpResult(id, w.bytes())

	case "tools/call":
		toolName, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if toolName == "" {
			return mcpError(200, id, -32602, "tools/call needs a tool name")
		}
		if mcpIsMutatingTool(toolName) {
			s.mu.Lock()
			defer s.mu.Unlock()
		} else if isMCPReadTool(toolName) {
			s.mu.RLock()
			defer s.mu.RUnlock()
		}
		outcome := mcpCallTool(s, toolName, args)
		switch outcome.kind {
		case "rpcError":
			return mcpError(200, id, outcome.code, outcome.message)
		case "toolError":
			var w jw
			w.raw(`{"content":[{"type":"text","text":`)
			w.str(outcome.message)
			w.raw(`}],"structuredContent":{"error":`)
			w.str(outcome.message)
			w.raw(`},"isError":true}`)
			return mcpResult(id, w.bytes())
		default:
			textBytes, _ := json.Marshal(string(outcome.result))
			var w jw
			w.raw(`{"content":[{"type":"text","text":`)
			w.buf = append(w.buf, textBytes...)
			w.raw(`}],"structuredContent":`)
			w.buf = append(w.buf, outcome.result...)
			w.raw(`,"isError":false}`)
			return mcpResult(id, w.bytes())
		}

	default:
		return mcpError(200, id, -32601, "Method not found: "+method)
	}
}

func isMCPReadTool(name string) bool {
	switch name {
	case "get_repo", "list_repos", "list_issues", "get_issue":
		return true
	}
	return false
}

func jsonStringArray(items []string) []byte {
	var w jw
	w.raw(`[`)
	for i, item := range items {
		if i > 0 {
			w.raw(`,`)
		}
		w.str(item)
	}
	w.raw(`]`)
	return w.bytes()
}

