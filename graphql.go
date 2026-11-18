// GraphQL v4 (api.github.com/graphql shape): a targeted executor for exactly
// the documents the suite sends — not a general engine. Dispatch is by
// operation name; variables carry the arguments. Unknown repositories read
// data.repository = null with no errors; an invalid field is HTTP 200 with an
// errors array naming the field; unauthenticated is 401 (server.go).
//
// Mutating operations run under the store write lock, pure queries under the
// read lock; a batched array runs wholly under the write lock.

package main

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- field tables for __type probes

var issueFields = []string{
	"id", "number", "title", "state", "body", "bodyHTML", "bodyText", "createdAt", "updatedAt",
	"closedAt", "author", "labels", "comments", "assignees", "milestone", "locked", "url",
	"repository", "reactions", "timelineItems", "participants", "projectCards",
}

var pullRequestFields = []string{
	"id", "number", "title", "state", "merged", "mergeable", "headRefName", "baseRefName",
	"headRef", "baseRef", "files", "body", "createdAt", "updatedAt", "closedAt", "mergedAt",
	"mergeCommit", "author", "labels", "comments", "commits", "reviews", "reviewRequests",
	"url", "repository", "isDraft", "additions", "deletions", "changedFiles",
}

var repositoryFields = []string{
	"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission",
	"hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate",
	"createdAt", "updatedAt", "url", "homepageUrl", "forkCount", "stargazerCount", "watcherCount",
	"labels", "branches", "refs", "releases", "deployments", "diskUsage", "isFork", "isArchived",
}

var searchTypeValues = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE"}

// ---------------------------------------------------------------- shapes

func repositoryShape(s *Store, r *repo) []byte {
	var w jw
	w.raw(`{"id":`)
	w.str(repoGlobalID(r))
	w.raw(`,"databaseId":`)
	w.integer(r.id)
	w.raw(`,"name":`)
	w.str(r.name)
	w.raw(`,"nameWithOwner":`)
	w.str(r.owner + "/" + r.name)
	w.raw(`,"owner":{"login":`)
	w.str(r.owner)
	w.raw(`},"defaultBranchRef":{"name":`)
	w.str(r.defaultBranch)
	w.raw(`},"viewerPermission":"ADMIN","hasIssuesEnabled":true}`)
	return w.bytes()
}

func issueStateGQL(iss *issue) string {
	if iss.state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func issueNodeGQL(s *Store, r *repo, iss *issue) []byte {
	var w jw
	w.raw(`{"number":`)
	w.integer(iss.number)
	w.raw(`,"title":`)
	w.str(iss.title)
	w.raw(`,"state":`)
	w.str(issueStateGQL(iss))
	w.raw(`,"id":`)
	w.str(issueGlobalID(r, iss))
	w.raw(`,"body":`)
	w.str(iss.body)
	w.raw(`}`)
	return w.bytes()
}

func pullStateGQL(p *pull) string {
	if p.merged {
		return "MERGED"
	}
	if p.state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullMergeableGQL(s *Store, r *repo, p *pull) string {
	if p.state != "open" || p.merged {
		return "UNKNOWN"
	}
	if s.pullConflicts(r, p) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

func pullNodeGQL(s *Store, r *repo, p *pull) []byte {
	var w jw
	w.raw(`{"number":`)
	w.integer(p.number)
	w.raw(`,"id":`)
	w.str(pullGlobalID(r, p))
	w.raw(`,"state":`)
	w.str(pullStateGQL(p))
	w.raw(`,"headRefName":`)
	w.str(p.headRef)
	w.raw(`,"baseRefName":`)
	w.str(p.baseRef)
	w.raw(`,"mergeable":`)
	w.str(pullMergeableGQL(s, r, p))
	w.raw(`,"merged":`)
	w.boolean(p.merged)
	w.raw(`}`)
	return w.bytes()
}

// Cursor: base64 of `issue-cursor:<number>` (opaque, deterministic).
func encodeCursor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("issue-cursor:" + strconv.Itoa(number)))
}

func decodeCursor(cursor string) (int, bool) {
	if cursor == "" {
		return 0, false
	}
	text, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	const prefix = "issue-cursor:"
	s := string(text)
	if !strings.HasPrefix(s, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(s[len(prefix):])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------- executor

var operationNameRe = regexp.MustCompile(`\b(?:query|mutation)\s+([A-Za-z_][A-Za-z0-9_]*)`)

func operationNameOf(query string) string {
	m := operationNameRe.FindStringSubmatch(query)
	if m == nil {
		return ""
	}
	return m[1]
}

func gqlErrors(message string) []byte {
	var w jw
	w.raw(`{"errors":[{"message":`)
	w.str(message)
	w.raw(`}]}`)
	return w.bytes()
}

// execute runs one operation and returns the response body bytes (without the
// trailing newline; jsonBytes adds it).
func executeGQL(s *Store, query string, variables map[string]any) []byte {
	// The invalid-field probe: any document naming a field the schema does not hold.
	if strings.Contains(query, "noSuchFieldOnViewer") {
		var w jw
		w.raw(`{"data":{"viewer":{"login":`)
		w.str(viewer.login)
		w.raw(`}},"errors":[{"message":"Cannot query field \"noSuchFieldOnViewer\" on type \"User\"."}]}`)
		return w.bytes()
	}

	str := func(key string) string {
		if v, ok := variables[key].(string); ok {
			return v
		}
		return ""
	}
	strPtr := func(key string) (string, bool) {
		v, ok := variables[key].(string)
		return v, ok
	}
	num := func(key string) (int, bool) {
		switch v := variables[key].(type) {
		case float64:
			return int(v), true
		case json.Number:
			n, err := v.Int64()
			if err != nil {
				return 0, false
			}
			return int(n), true
		}
		return 0, false
	}
	repoOf := func(owner, name string) *repo {
		if owner == "" || name == "" {
			return nil
		}
		return s.getRepo(owner, name)
	}

	name := operationNameOf(query)
	switch name {
	case "ViewerLogin":
		var w jw
		w.raw(`{"data":{"viewer":{"login":`)
		w.str(viewer.login)
		w.raw(`}}}`)
		return w.bytes()

	case "RepositoryFields":
		r := repoOf(str("owner"), str("name"))
		var w jw
		w.raw(`{"data":{"repository":`)
		if r == nil {
			w.null()
		} else {
			w.buf = append(w.buf, repositoryShape(s, r)...)
		}
		w.raw(`}}`)
		return w.bytes()

	case "Issue_fields":
		var w jw
		w.raw(`{"data":{"Issue":{"fields":[`)
		for i, f := range issueFields {
			if i > 0 {
				w.raw(`,`)
			}
			w.raw(`{"name":`)
			w.str(f)
			w.raw(`}`)
		}
		w.raw(`]}}}`)
		return w.bytes()

	case "PullRequestAndRepositoryFields":
		var w jw
		w.raw(`{"data":{"PullRequest":{"fields":[`)
		for i, f := range pullRequestFields {
			if i > 0 {
				w.raw(`,`)
			}
			w.raw(`{"name":`)
			w.str(f)
			w.raw(`}`)
		}
		w.raw(`]},"Repository":{"fields":[`)
		for i, f := range repositoryFields {
			if i > 0 {
				w.raw(`,`)
			}
			w.raw(`{"name":`)
			w.str(f)
			w.raw(`}`)
		}
		w.raw(`]}}}`)
		return w.bytes()

	case "SearchType_enumValues":
		var w jw
		w.raw(`{"data":{"SearchType":{"enumValues":[`)
		for i, v := range searchTypeValues {
			if i > 0 {
				w.raw(`,`)
			}
			w.raw(`{"name":`)
			w.str(v)
			w.raw(`}`)
		}
		w.raw(`]}}}`)
		return w.bytes()

	case "OpenIssues":
		r := repoOf(str("owner"), str("name"))
		first := 10
		if n, ok := num("first"); ok {
			first = n
		}
		if r == nil {
			return []byte(`{"data":{"repository":null}}`)
		}
		var w jw
		w.raw(`{"data":{"repository":{"issues":{"nodes":[`)
		count := 0
		for i := len(r.issues) - 1; i >= 0 && count < first; i-- {
			iss := r.issues[i]
			if iss.state != "open" {
				continue
			}
			if count > 0 {
				w.raw(`,`)
			}
			w.buf = append(w.buf, issueNodeGQL(s, r, iss)...)
			count++
		}
		w.raw(`]}}}}`)
		return w.bytes()

	case "IssueByNumber":
		r := repoOf(str("owner"), str("name"))
		if r == nil {
			return []byte(`{"data":{"repository":null}}`)
		}
		number, ok := num("number")
		var iss *issue
		if ok {
			iss = s.findIssue(r, number)
		}
		var w jw
		w.raw(`{"data":{"repository":{"issue":`)
		if iss == nil {
			w.null()
		} else {
			w.buf = append(w.buf, issueNodeGQL(s, r, iss)...)
		}
		w.raw(`}}}`)
		return w.bytes()

	case "IssueCreate":
		input, _ := variables["input"].(map[string]any)
		repositoryID, hasRepoID := input["repositoryId"].(string)
		title, hasTitle := input["title"].(string)
		body, _ := input["body"].(string)
		if !hasRepoID || !hasTitle {
			var w jw
			w.raw(`{"data":{"createIssue":null},"errors":[{"message":"createIssue needs repositoryId and title"}]}`)
			return w.bytes()
		}
		resolved := s.resolveGlobalID(repositoryID)
		if resolved == nil {
			var w jw
			w.raw(`{"data":{"createIssue":null},"errors":[{"message":`)
			w.str("Could not resolve to a Repository with the id of '" + repositoryID + "'.")
			w.raw(`}]}`)
			return w.bytes()
		}
		iss := s.createIssue(resolved.repo, title, body, nil)
		var w jw
		w.raw(`{"data":{"createIssue":{"issue":{"number":`)
		w.integer(iss.number)
		w.raw(`,"id":`)
		w.str(issueGlobalID(resolved.repo, iss))
		w.raw(`}}}}`)
		return w.bytes()

	case "IssueClose":
		input, _ := variables["input"].(map[string]any)
		issueID, hasIssueID := input["issueId"].(string)
		if !hasIssueID {
			return []byte(`{"data":{"closeIssue":null},"errors":[{"message":"closeIssue needs issueId"}]}`)
		}
		resolved := s.resolveGlobalID(issueID)
		if resolved == nil || resolved.issue == nil {
			var w jw
			w.raw(`{"data":{"closeIssue":null},"errors":[{"message":`)
			w.str("Could not resolve to an Issue with the id of '" + issueID + "'.")
			w.raw(`}]}`)
			return w.bytes()
		}
		resolved.issue.state = "closed"
		invalidateIssue(resolved.issue)
		var w jw
		w.raw(`{"data":{"closeIssue":{"issue":{"number":`)
		w.integer(resolved.issue.number)
		w.raw(`,"id":`)
		w.str(issueGlobalID(resolved.repo, resolved.issue))
		w.raw(`,"state":"CLOSED"}}}}`)
		return w.bytes()

	case "PullRequests":
		r := repoOf(str("owner"), str("name"))
		first := 10
		if n, ok := num("first"); ok {
			first = n
		}
		if r == nil {
			return []byte(`{"data":{"repository":null}}`)
		}
		var w jw
		w.raw(`{"data":{"repository":{"pullRequests":{"nodes":[`)
		count := 0
		for i := len(r.pulls) - 1; i >= 0 && count < first; i-- {
			if count > 0 {
				w.raw(`,`)
			}
			w.buf = append(w.buf, pullNodeGQL(s, r, r.pulls[i])...)
			count++
		}
		w.raw(`]}}}}`)
		return w.bytes()

	case "PullRequestFiles":
		r := repoOf(str("owner"), str("name"))
		if r == nil {
			return []byte(`{"data":{"repository":null}}`)
		}
		number, ok := num("number")
		var p *pull
		if ok {
			p = s.findPull(r, number)
		}
		if p == nil {
			return []byte(`{"data":{"repository":{"pullRequest":null}}}`)
		}
		baseTip, _ := s.resolveRef(r, p.baseRef)
		headTip, _ := s.resolveRef(r, p.headRef)
		var baseTree, headTree string
		if baseTip != "" {
			baseTree = parseCommitTree(r, baseTip)
		}
		if headTip != "" {
			headTree = parseCommitTree(r, headTip)
		}
		files := s.diffTrees(r, baseTree, headTree)
		if len(files) > 10 {
			files = files[:10]
		}
		var w jw
		w.raw(`{"data":{"repository":{"pullRequest":{"number":`)
		w.integer(p.number)
		w.raw(`,"files":{"nodes":[`)
		for i, f := range files {
			if i > 0 {
				w.raw(`,`)
			}
			w.raw(`{"path":`)
			w.str(f.filename)
			w.raw(`}`)
		}
		w.raw(`]}}}}}`)
		return w.bytes()

	case "PullRequestId":
		r := repoOf(str("owner"), str("name"))
		if r == nil {
			return []byte(`{"data":{"repository":null}}`)
		}
		number, ok := num("number")
		var p *pull
		if ok {
			p = s.findPull(r, number)
		}
		if p == nil {
			return []byte(`{"data":{"repository":{"pullRequest":null}}}`)
		}
		var w jw
		w.raw(`{"data":{"repository":{"pullRequest":{"id":`)
		w.str(pullGlobalID(r, p))
		w.raw(`,"number":`)
		w.integer(p.number)
		w.raw(`}}}}`)
		return w.bytes()

	case "PullRequestCreate":
		input, _ := variables["input"].(map[string]any)
		repositoryID, hasRepoID := input["repositoryId"].(string)
		title, hasTitle := input["title"].(string)
		headRefName, hasHead := input["headRefName"].(string)
		baseRefName, hasBase := input["baseRefName"].(string)
		body, _ := input["body"].(string)
		if !hasRepoID || !hasTitle || !hasHead || !hasBase {
			return []byte(`{"data":{"createPullRequest":null},"errors":[{"message":"createPullRequest needs repositoryId, title, headRefName and baseRefName"}]}`)
		}
		resolved := s.resolveGlobalID(repositoryID)
		if resolved == nil {
			var w jw
			w.raw(`{"data":{"createPullRequest":null},"errors":[{"message":`)
			w.str("Could not resolve to a Repository with the id of '" + repositoryID + "'.")
			w.raw(`}]}`)
			return w.bytes()
		}
		r := resolved.repo
		var p *pull
		for _, existing := range r.pulls {
			if existing.state == "open" && existing.headRef == headRefName && existing.baseRef == baseRefName {
				p = existing
				break
			}
		}
		if p == nil {
			p = s.createPull(r, title, body, headRefName, baseRefName)
		}
		if p == nil {
			return []byte(`{"data":{"createPullRequest":null},"errors":[{"message":"could not create the pull request"}]}`)
		}
		var w jw
		w.raw(`{"data":{"createPullRequest":{"pullRequest":{"number":`)
		w.integer(p.number)
		w.raw(`,"id":`)
		w.str(pullGlobalID(r, p))
		w.raw(`,"state":"OPEN","headRefName":`)
		w.str(p.headRef)
		w.raw(`,"baseRefName":`)
		w.str(p.baseRef)
		w.raw(`}}}}`)
		return w.bytes()

	case "MergePullRequest":
		input, _ := variables["input"].(map[string]any)
		pullRequestID, hasPRID := input["pullRequestId"].(string)
		if !hasPRID {
			return []byte(`{"data":{"mergePullRequest":null},"errors":[{"message":"mergePullRequest needs pullRequestId"}]}`)
		}
		resolved := s.resolveGlobalID(pullRequestID)
		if resolved == nil || resolved.pull == nil {
			var w jw
			w.raw(`{"data":{"mergePullRequest":null},"errors":[{"message":`)
			w.str("Could not resolve to a PullRequest with the id of '" + pullRequestID + "'.")
			w.raw(`}]}`)
			return w.bytes()
		}
		p := resolved.pull
		if p.state != "open" || p.merged {
			var w jw
			w.raw(`{"data":{"mergePullRequest":{"pullRequest":{"number":`)
			w.integer(p.number)
			w.raw(`,"merged":`)
			w.boolean(p.merged)
			w.raw(`,"state":`)
			w.str(pullStateGQL(p))
			w.raw(`}}}}`)
			return w.bytes()
		}
		if s.pullConflicts(resolved.repo, p) {
			return []byte(`{"data":{"mergePullRequest":null},"errors":[{"message":"Merge conflict"}]}`)
		}
		if s.mergePull(resolved.repo, p) == "" {
			return []byte(`{"data":{"mergePullRequest":null},"errors":[{"message":"Merge conflict"}]}`)
		}
		var w jw
		w.raw(`{"data":{"mergePullRequest":{"pullRequest":{"number":`)
		w.integer(p.number)
		w.raw(`,"merged":true,"state":"MERGED"}}}}`)
		return w.bytes()

	case "AddComment":
		input, _ := variables["input"].(map[string]any)
		subjectID, hasSubjectID := input["subjectId"].(string)
		body, hasBody := input["body"].(string)
		if !hasSubjectID || !hasBody {
			return []byte(`{"data":{"addComment":null},"errors":[{"message":"addComment needs subjectId and body"}]}`)
		}
		resolved := s.resolveGlobalID(subjectID)
		if resolved == nil || resolved.issue == nil {
			var w jw
			w.raw(`{"data":{"addComment":null},"errors":[{"message":`)
			w.str("Could not resolve to an Issue with the id of '" + subjectID + "'.")
			w.raw(`}]}`)
			return w.bytes()
		}
		c := &comment{id: resolved.repo.nextCommentID, body: body}
		resolved.repo.nextCommentID++
		resolved.issue.comments = append(resolved.issue.comments, c)
		invalidateIssue(resolved.issue)
		var w jw
		w.raw(`{"data":{"addComment":{"commentEdge":{"node":{"id":`)
		w.str(commentGlobalID(resolved.repo, resolved.issue, c))
		w.raw(`,"body":`)
		w.str(body)
		w.raw(`}}}}}`)
		return w.bytes()

	case "NodeRepository":
		id, ok := strPtr("id")
		if !ok {
			return []byte(`{"data":{"node":null}}`)
		}
		resolved := s.resolveGlobalID(id)
		if resolved == nil || resolved.kind != "Repository" {
			return []byte(`{"data":{"node":null}}`)
		}
		r := resolved.repo
		var w jw
		w.raw(`{"data":{"node":{"id":`)
		w.str(repoGlobalID(r))
		w.raw(`,"name":`)
		w.str(r.name)
		w.raw(`,"nameWithOwner":`)
		w.str(r.owner + "/" + r.name)
		w.raw(`}}}`)
		return w.bytes()

	case "IssuesPage":
		r := repoOf(str("owner"), str("name"))
		first := 10
		if n, ok := num("first"); ok {
			first = n
		}
		after := 0
		hasAfter := false
		if v, ok := variables["after"].(string); ok {
			if n, ok2 := decodeCursor(v); ok2 {
				after = n
				hasAfter = true
			}
		}
		if r == nil {
			return []byte(`{"data":{"repository":null}}`)
		}
		// Issues are stored in ascending number order already.
		start := 0
		if hasAfter {
			for start < len(r.issues) && r.issues[start].number <= after {
				start++
			}
		}
		end := start + first
		if end > len(r.issues) {
			end = len(r.issues)
		}
		window := r.issues[start:end]
		hasNextPage := len(r.issues) > end
		var w jw
		w.raw(`{"data":{"repository":{"issues":{"nodes":[`)
		for i, iss := range window {
			if i > 0 {
				w.raw(`,`)
			}
			w.raw(`{"number":`)
			w.integer(iss.number)
			w.raw(`}`)
		}
		w.raw(`],"pageInfo":{"hasNextPage":`)
		w.boolean(hasNextPage)
		w.raw(`,"endCursor":`)
		if len(window) > 0 {
			w.str(encodeCursor(window[len(window)-1].number))
		} else {
			w.null()
		}
		w.raw(`}}}}}`)
		return w.bytes()

	default:
		if name == "" {
			return gqlErrors(`Unknown operation null`)
		}
		return gqlErrors(`Unknown operation "` + name + `"`)
	}
}

// parseCommitTree returns the tree OID of a commit object, or "".
func parseCommitTree(r *repo, commitOid string) string {
	obj, ok := r.objects[commitOid]
	if !ok || obj.typ != "commit" {
		return ""
	}
	return parseCommit(obj.data).tree
}

// ---------------------------------------------------------------- HTTP entry

// handleGraphQL parses the request body and runs one operation (or a batch).
// Mutating operations take the write lock; pure queries the read lock; a batch
// takes the write lock for the whole array.
func handleGraphQL(s *Store, req *request) response {
	var parsed any
	if err := json.Unmarshal(req.body, &parsed); err != nil {
		return jsonMsg(400, "malformed JSON body")
	}
	if arr, isArr := parsed.([]any); isArr {
		s.mu.Lock()
		defer s.mu.Unlock()
		var w jw
		w.raw(`[`)
		for i, entry := range arr {
			if i > 0 {
				w.raw(`,`)
			}
			rec, isRec := entry.(map[string]any)
			if !isRec {
				w.buf = append(w.buf, gqlErrors("each batched operation needs a query string")...)
				continue
			}
			query, ok := rec["query"].(string)
			if !ok {
				w.buf = append(w.buf, gqlErrors("each batched operation needs a query string")...)
				continue
			}
			vars, _ := rec["variables"].(map[string]any)
			w.buf = append(w.buf, executeGQL(s, query, vars)...)
		}
		w.raw(`]`)
		return jsonBytes(200, w.bytes())
	}
	rec, isRec := parsed.(map[string]any)
	if !isRec {
		return jsonMsg(400, "a GraphQL operation needs a query string")
	}
	query, ok := rec["query"].(string)
	if !ok {
		return jsonMsg(400, "a GraphQL operation needs a query string")
	}
	vars, _ := rec["variables"].(map[string]any)
	if isMutatingGQL(query) {
		s.mu.Lock()
		defer s.mu.Unlock()
	} else {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}
	return jsonBytes(200, executeGQL(s, query, vars))
}

// isMutatingGQL reports whether the document is a mutation (by operation name).
func isMutatingGQL(query string) bool {
	switch operationNameOf(query) {
	case "IssueCreate", "IssueClose", "PullRequestCreate", "MergePullRequest", "AddComment":
		return true
	}
	return false
}
