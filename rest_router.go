// REST v3 router (continued from rest.go): dispatch by method + segments.

package main

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
)

// handleRest routes one REST v3 request. GETs run under RLock, mutating
// methods under Lock.
func handleRest(s *Store, req *request) response {
	if req.method == "GET" {
		s.mu.RLock()
		defer s.mu.RUnlock()
	} else {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	return handleRestLocked(s, req)
}

func handleRestLocked(s *Store, req *request) response {
	query := req.query
	segments := []string{}
	for _, part := range strings.Split(req.path, "/") {
		if part != "" {
			segments = append(segments, decodeSegment(part))
		}
	}
	// Drop 'api' and 'v3'.
	if len(segments) >= 2 {
		segments = segments[2:]
	} else {
		segments = nil
	}
	method := req.method
	origin := "http://" + req.host
	path := req.path

	needRepo := func(owner, name string) (*repo, *response) {
		r := s.getRepo(owner, name)
		if r == nil {
			resp := jsonMsg(404, "Not Found")
			return nil, &resp
		}
		return r, nil
	};

	// GET /api/v3/ — the API root.
	if len(segments) == 0 {
		if method != "GET" {
			return jsonMsg(404, "Not Found")
		}
		var w jw
		w.raw(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`)
		resp := jsonBytes(200, w.bytes())
		resp.headers = map[string]string{"x-oauth-scopes": "repo, read:org"}
		return resp
	}

	// GET /api/v3/meta
	if len(segments) == 1 && segments[0] == "meta" {
		if method != "GET" {
			return jsonMsg(404, "Not Found")
		}
		var w jw
		w.raw(`{"installed_version":"1.0.0"}`)
		return jsonBytes(200, w.bytes())
	}

	// GET /api/v3/user
	if len(segments) == 1 && segments[0] == "user" {
		if method != "GET" {
			return jsonMsg(404, "Not Found")
		}
		return jsonBytes(200, userJSON())
	}

	// /api/v3/user/repos
	if len(segments) == 2 && segments[0] == "user" && segments[1] == "repos" {
		if method == "GET" {
			page, perPage := pageParams(query)
			all := s.listRepos(viewer.login)
			items := paginate(all, page, perPage)
			var w jw
			w.raw(`[`)
			for i, r := range items {
				if i > 0 {
					w.raw(`,`)
				}
				w.buf = append(w.buf, repoJSON(r)...)
			}
			w.raw(`]`)
			resp := jsonBytes(200, w.bytes())
			if link := linkHeader(origin, path, query, page, perPage, len(all)); link != "" {
				resp.headers = map[string]string{"link": link}
			}
			return resp
		}
		if method == "POST" {
			parsed, ok := parseJSONBody(req.body)
			if !ok {
				return jsonMsg(400, "malformed JSON body")
			}
			if !isRecord(parsed) {
				return jsonMsg(422, "Validation Failed")
			}
			rec := parsed.(map[string]any)
			name, _ := asString(rec["name"])
			if name == "" || strings.ContainsAny(name, "/ \t\n\r") {
				return jsonMsg(422, "name is required")
			}
			if s.getRepo(viewer.login, name) != nil {
				return jsonMsg(422, "Repository already exists")
			}
			description, _ := asString(rec["description"])
			isPrivate, _ := rec["private"].(bool)
			r := s.createRepo(viewer.login, name, description, isPrivate)
			return jsonBytes(201, repoJSON(r))
		}
		return jsonMsg(404, "Not Found")
	}

	// /api/v3/repos/{owner}/{name}[...]
	if len(segments) >= 3 && segments[0] == "repos" {
		owner := segments[1]
		name := segments[2]
		rest := segments[3:]

		// The repo itself.
		if len(rest) == 0 {
			if method == "GET" {
				r, errResp := needRepo(owner, name)
				if errResp != nil {
					return *errResp
				}
				return jsonBytes(200, repoJSON(r))
			}
			if method == "PATCH" {
				r, errResp := needRepo(owner, name)
				if errResp != nil {
					return *errResp
				}
				parsed, ok := parseJSONBody(req.body)
				if !ok {
					return jsonMsg(400, "malformed JSON body")
				}
				if rec, isRec := parsed.(map[string]any); isRec {
					if desc, ok := rec["description"].(string); ok {
						r.description = desc
					}
					if priv, ok := rec["private"].(bool); ok {
						r.isPrivate = priv
					}
				}
				return jsonBytes(200, repoJSON(r))
			}
			if method == "DELETE" {
				r, errResp := needRepo(owner, name)
				if errResp != nil {
					return *errResp
				}
				_ = r
				s.deleteRepo(owner, name)
				return emptyResp(204)
			}
			return jsonMsg(404, "Not Found")
		}

		// git/refs
		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" && len(rest) == 2 && method == "POST" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			parsed, ok := parseJSONBody(req.body)
			if !ok {
				return jsonMsg(400, "malformed JSON body")
			}
			if !isRecord(parsed) {
				return jsonMsg(422, "Validation Failed")
			}
			rec := parsed.(map[string]any)
			ref, _ := asString(rec["ref"])
			sha, _ := asString(rec["sha"])
			if ref == "" || !strings.HasPrefix(ref, "refs/") || sha == "" || !isHex40(sha) {
				return jsonMsg(422, "Invalid ref or sha")
			}
			if _, ok := r.objects[sha]; !ok {
				return jsonMsg(422, "Invalid SHA")
			}
			if _, ok := r.refs[ref]; ok {
				return jsonMsg(422, "Reference already exists")
			}
			r.refs[ref] = sha
			typ := "commit"
			if obj, ok := r.objects[sha]; ok {
				typ = obj.typ
			}
			var w jw
			w.raw(`{"ref":`)
			w.str(ref)
			w.raw(`,"node_id":"","object":{"sha":`)
			w.str(sha)
			w.raw(`,"type":`)
			w.str(typ)
			w.raw(`,"url":""},"url":""}`)
			return jsonBytes(201, w.bytes())
		}
		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" && len(rest) >= 3 && rest[2] == "heads" && len(rest) >= 4 && method == "GET" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			branch := strings.Join(rest[3:], "/")
			sha, ok := r.refs["refs/heads/"+branch]
			if !ok {
				return jsonMsg(404, "Not Found")
			}
			typ := "commit"
			if obj, ok := r.objects[sha]; ok {
				typ = obj.typ
			}
			var w jw
			w.raw(`{"ref":`)
			w.str("refs/heads/" + branch)
			w.raw(`,"node_id":"","object":{"sha":`)
			w.str(sha)
			w.raw(`,"type":`)
			w.str(typ)
			w.raw(`,"url":""},"url":""}`)
			return jsonBytes(200, w.bytes())
		}

		// branches
		if rest[0] == "branches" && len(rest) == 1 && method == "GET" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			var w jw
			w.raw(`[`)
			for i, branch := range s.branches(r) {
				if i > 0 {
					w.raw(`,`)
				}
				w.raw(`{"name":`)
				w.str(branch.name)
				w.raw(`,"commit":{"sha":`)
				w.str(branch.sha)
				w.raw(`,"url":""},"protected":false}`)
			}
			w.raw(`]`)
			return jsonBytes(200, w.bytes())
		}
		if rest[0] == "branches" && len(rest) >= 2 && method == "GET" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			branch := strings.Join(rest[1:], "/")
			sha, ok := r.refs["refs/heads/"+branch]
			if !ok {
				return jsonMsg(404, "Not Found")
			}
			var w jw
			w.raw(`{"name":`)
			w.str(branch)
			w.raw(`,"commit":{"sha":`)
			w.str(sha)
			w.raw(`,"url":""},"protected":false}`)
			return jsonBytes(200, w.bytes())
		}

		// commits
		if rest[0] == "commits" && len(rest) == 1 && method == "GET" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			ref := query.Get("sha")
			if ref == "" {
				ref = r.defaultBranch
			}
			commits := s.listCommits(r, ref)
			if len(commits) == 0 {
				if _, ok := s.resolveRef(r, ref); !ok {
					if _, ok := r.objects[ref]; !ok {
						return jsonMsg(404, "No commit found for SHA")
					}
				}
			}
			var w jw
			w.raw(`[`)
			for i, entry := range commits {
				if i > 0 {
					w.raw(`,`)
				}
				authorName, authorEmail, authorDate := parseAuthorLine(entry.commit.author)
				committerName, committerEmail, committerDate := parseAuthorLine(entry.commit.committer)
				w.raw(`{"sha":`)
				w.str(entry.sha)
				w.raw(`,"commit":{"message":`)
				w.str(entry.commit.message)
				w.raw(`,"author":{"name":`)
				w.str(authorName)
				w.raw(`,"email":`)
				w.str(authorEmail)
				w.raw(`,"date":`)
				w.str(authorDate)
				w.raw(`},"committer":{"name":`)
				w.str(committerName)
				w.raw(`,"email":`)
				w.str(committerEmail)
				w.raw(`,"date":`)
				w.str(committerDate)
				w.raw(`}},"author":{"login":`)
				w.str(viewer.login)
				w.raw(`},"committer":{"login":`)
				w.str(viewer.login)
				w.raw(`},"parents":[`)
				for j, parent := range entry.commit.parents {
					if j > 0 {
						w.raw(`,`)
					}
					w.raw(`{"sha":`)
					w.str(parent)
					w.raw(`}`)
				}
				w.raw(`]}`)
			}
			w.raw(`]`)
			return jsonBytes(200, w.bytes())
		}

		// contents
		if rest[0] == "contents" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			path := strings.Join(rest[1:], "/")
			if method == "GET" {
				ref := query.Get("ref")
				if ref == "" {
					ref = r.defaultBranch
				}
				read := s.readPath(r, ref, path)
				if read == nil {
					return jsonMsg(404, "Not Found")
				}
				if read.kind == "file" {
					var w jw
					w.raw(`{"type":"file","encoding":"base64","size":`)
					w.integer(len(read.data))
					w.raw(`,"name":`)
					namePart := path
					if idx := strings.LastIndex(path, "/"); idx >= 0 {
						namePart = path[idx+1:]
					}
					w.str(namePart)
					w.raw(`,"path":`)
					w.str(path)
					w.raw(`,"content":`)
					w.str(base64.StdEncoding.EncodeToString(read.data))
					w.raw(`,"sha":`)
					w.str(read.blobOid)
					w.raw(`}`)
					return jsonBytes(200, w.bytes())
				}
				var w jw
				w.raw(`[`)
				for i, entry := range read.entries {
					if i > 0 {
						w.raw(`,`)
					}
					size := 0
					if entry.mode != "40000" {
						if obj, ok := r.objects[entry.oid]; ok {
							size = len(obj.data)
						}
					}
					entryPath := entry.name
					if path != "" {
						entryPath = path + "/" + entry.name
					}
					w.raw(`{"type":`)
					if entry.mode == "40000" {
						w.str("dir")
					} else {
						w.str("file")
					}
					w.raw(`,"size":`)
					w.integer(size)
					w.raw(`,"name":`)
					w.str(entry.name)
					w.raw(`,"path":`)
					w.str(entryPath)
					w.raw(`,"sha":`)
					w.str(entry.oid)
					w.raw(`}`)
				}
				w.raw(`]`)
				return jsonBytes(200, w.bytes())
			}
			if method == "PUT" {
				parsed, ok := parseJSONBody(req.body)
				if !ok {
					return jsonMsg(400, "malformed JSON body")
				}
				if !isRecord(parsed) {
					return jsonMsg(422, "Validation Failed")
				}
				rec := parsed.(map[string]any)
				messageText, _ := asString(rec["message"])
				if messageText == "" {
					messageText = "Update file"
				}
				contentB64, _ := asString(rec["content"])
				if contentB64 == "" {
					// Distinguish missing (null) from empty: TS checks === null.
					if _, present := rec["content"]; !present {
						return jsonMsg(422, "content is required")
					}
				}
				content, err := base64.StdEncoding.DecodeString(contentB64)
				if err != nil {
					return jsonMsg(422, "content is not valid base64")
				}
				branch, _ := asString(rec["branch"])
				if branch == "" {
					branch = r.defaultBranch
				}
				sha, _ := asString(rec["sha"])
				ref := "refs/heads/" + branch
				tip, ok := r.refs[ref]
				if !ok {
					if branch == r.defaultBranch {
						tip = ""
					} else {
						tip, _ = r.refs["refs/heads/"+r.defaultBranch]
					}
				}
				var baseTree string
				if tip != "" {
					baseTree = parseCommit(r.objects[tip].data).tree
				}
				flat := s.flattenTree(r, baseTree)
				existing, exists := flat[path]
				if exists {
					if sha == "" {
						return jsonMsg(422, "sha is required to update a file")
					}
					if sha != existing.oid {
						return jsonMsg(409, "sha does not match the current blob")
					}
				} else if sha != "" {
					return jsonMsg(422, "sha does not match any existing file")
				}
				commitOid := s.commitFile(r, branch, path, content, messageText)
				blobOid := s.putObject(r, "blob", content)
				isCreate := !exists
				var w jw
				w.raw(`{"content":{"name":`)
				namePart := path
				if idx := strings.LastIndex(path, "/"); idx >= 0 {
					namePart = path[idx+1:]
				}
				w.str(namePart)
				w.raw(`,"path":`)
				w.str(path)
				w.raw(`,"sha":`)
				w.str(blobOid)
				w.raw(`,"size":`)
				w.integer(len(content))
				w.raw(`,"type":"file","encoding":"base64","content":`)
				w.str(base64.StdEncoding.EncodeToString(content))
				w.raw(`},"commit":{"sha":`)
				w.str(commitOid)
				w.raw(`,"message":`)
				w.str(messageText)
				w.raw(`}}`)
				if isCreate {
					return jsonBytes(201, w.bytes())
				}
				return jsonBytes(200, w.bytes())
			}
			if method == "DELETE" {
				parsed, ok := parseJSONBody(req.body)
				if !ok {
					return jsonMsg(400, "malformed JSON body")
				}
				if !isRecord(parsed) {
					return jsonMsg(422, "Validation Failed")
				}
				rec := parsed.(map[string]any)
				messageText, _ := asString(rec["message"])
				if messageText == "" {
					messageText = "Delete file"
				}
				sha, _ := asString(rec["sha"])
				branch, _ := asString(rec["branch"])
				if branch == "" {
					branch = r.defaultBranch
				}
				ref := "refs/heads/" + branch
				tip, ok := r.refs[ref]
				if !ok {
					return jsonMsg(404, "Not Found")
				}
				baseTree := parseCommit(r.objects[tip].data).tree
				flat := s.flattenTree(r, baseTree)
				existing, exists := flat[path]
				if !exists {
					return jsonMsg(404, "Not Found")
				}
				if sha != "" && sha != existing.oid {
					return jsonMsg(409, "sha does not match the current blob")
				}
				commitOid := s.deleteFile(r, branch, path, messageText)
				if commitOid == "" {
					return jsonMsg(404, "Not Found")
				}
				var w jw
				w.raw(`{"commit":{"sha":`)
				w.str(commitOid)
				w.raw(`,"message":`)
				w.str(messageText)
				w.raw(`},"content":null}`)
				return jsonBytes(200, w.bytes())
			}
			return jsonMsg(404, "Not Found")
		}

		// labels
		if rest[0] == "labels" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			if len(rest) == 1 && method == "GET" {
				var w jw
				w.raw(`[`)
				for i, labelName := range r.labelOrder {
					if i > 0 {
						w.raw(`,`)
					}
					w.buf = append(w.buf, labelJSON(r, labelName)...)
				}
				w.raw(`]`)
				return jsonBytes(200, w.bytes())
			}
			if len(rest) == 1 && method == "POST" {
				parsed, ok := parseJSONBody(req.body)
				if !ok {
					return jsonMsg(400, "malformed JSON body")
				}
				if !isRecord(parsed) {
					return jsonMsg(422, "Validation Failed")
				}
				rec := parsed.(map[string]any)
				labelName, _ := asString(rec["name"])
				if labelName == "" {
					return jsonMsg(422, "name is required")
				}
				if _, ok := r.labels[labelName]; ok {
					return jsonMsg(422, "Label already exists")
				}
				color, _ := asString(rec["color"])
				if color == "" {
					color = "ededed"
				}
				description, _ := asString(rec["description"])
				r.labels[labelName] = &label{name: labelName, color: color, description: description}
				r.labelOrder = append(r.labelOrder, labelName)
				// A new label can change cached issue JSONs that name it.
				for _, iss := range r.issues {
					invalidateIssue(iss)
				}
				return jsonBytes(201, labelJSON(r, labelName))
			}
			return jsonMsg(404, "Not Found")
		}

		// issues
		if rest[0] == "issues" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			if len(rest) == 1 && method == "GET" {
				state := query.Get("state")
				if state == "" {
					state = "open"
				}
				sort := query.Get("sort")
				if sort == "" {
					sort = "created"
				}
				direction := query.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				items := append([]*issue(nil), r.issues...)
				if state != "all" {
					filtered := items[:0]
					for _, iss := range items {
						if iss.state == state {
							filtered = append(filtered, iss)
						}
					}
					items = filtered
				}
				if sort == "created" {
					// Sort by number (creation order proxy), direction-aware.
					for i := 0; i < len(items); i++ {
						for j := i + 1; j < len(items); j++ {
							swap := false
							if direction == "asc" {
								swap = items[j].number < items[i].number
							} else {
								swap = items[j].number > items[i].number
							}
							if swap {
								items[i], items[j] = items[j], items[i]
							}
						}
					}
				}
				page, perPage := pageParams(query)
				items = paginate(items, page, perPage)
				var w jw
				w.raw(`[`)
				for i, iss := range items {
					if i > 0 {
						w.raw(`,`)
					}
					w.buf = append(w.buf, issueJSONBytes(r, iss, nil)...)
				}
				w.raw(`]`)
				return jsonBytes(200, w.bytes())
			}
			if len(rest) == 1 && method == "POST" {
				parsed, ok := parseJSONBody(req.body)
				if !ok {
					return jsonMsg(400, "malformed JSON body")
				}
				if !isRecord(parsed) {
					return jsonMsg(422, "Validation Failed")
				}
				rec := parsed.(map[string]any)
				title, _ := asString(rec["title"])
				if title == "" {
					return jsonMsg(422, "title is required")
				}
				bodyText, _ := asString(rec["body"])
				var labelNames []string
				if arr, isArr := rec["labels"].([]any); isArr {
					for _, item := range arr {
						if str, ok := item.(string); ok {
							labelNames = append(labelNames, str)
						} else if rec2, ok := item.(map[string]any); ok {
							if n, ok := rec2["name"].(string); ok {
								labelNames = append(labelNames, n)
							}
						}
					}
				}
				for _, labelName := range labelNames {
					if _, ok := r.labels[labelName]; !ok {
						return jsonMsg(422, "Label '"+labelName+"' does not exist")
					}
				}
				iss := s.createIssue(r, title, bodyText, labelNames)
				return jsonBytes(201, issueJSONBytes(r, iss, nil))
			}
			if len(rest) == 2 {
				number, err := strconv.Atoi(rest[1])
				if err != nil || number < 1 {
					return jsonMsg(404, "Not Found")
				}
				if method == "GET" {
					if iss := s.findIssue(r, number); iss != nil {
						return jsonBytes(200, issueJSONBytes(r, iss, nil))
					}
					if p := s.findPull(r, number); p != nil {
						// A pull request read through the issues view.
						pseudo := &issue{id: p.id, number: p.number, title: p.title, body: p.body, state: p.state}
						return jsonBytes(200, issueJSONBytes(r, pseudo, p))
					}
					return jsonMsg(404, "Not Found")
				}
				if method == "PATCH" {
					iss := s.findIssue(r, number)
					if iss == nil {
						return jsonMsg(404, "Not Found")
					}
					parsed, ok := parseJSONBody(req.body)
					if !ok {
						return jsonMsg(400, "malformed JSON body")
					}
					if rec, isRec := parsed.(map[string]any); isRec {
						changed := false
						if st, ok := rec["state"].(string); ok && (st == "open" || st == "closed") && st != iss.state {
							iss.state = st
							changed = true
						}
						if t, ok := rec["title"].(string); ok && t != iss.title {
							iss.title = t
							changed = true
						}
						if b, ok := rec["body"].(string); ok && b != iss.body {
							iss.body = b
							changed = true
						}
						if changed {
							invalidateIssue(iss)
						}
					}
					return jsonBytes(200, issueJSONBytes(r, iss, nil))
				}
				return jsonMsg(404, "Not Found")
			}
			if len(rest) == 3 && rest[2] == "comments" {
				number, err := strconv.Atoi(rest[1])
				if err != nil || number < 1 {
					return jsonMsg(404, "Not Found")
				}
				iss := s.findIssue(r, number)
				if iss == nil {
					return jsonMsg(404, "Not Found")
				}
				if method == "GET" {
					var w jw
					w.raw(`[`)
					for i, c := range iss.comments {
						if i > 0 {
							w.raw(`,`)
						}
						w.buf = append(w.buf, commentJSON(r, iss, c)...)
					}
					w.raw(`]`)
					return jsonBytes(200, w.bytes())
				}
				if method == "POST" {
					parsed, ok := parseJSONBody(req.body)
					if !ok {
						return jsonMsg(400, "malformed JSON body")
					}
					if !isRecord(parsed) {
						return jsonMsg(422, "Validation Failed")
					}
					rec := parsed.(map[string]any)
					text, _ := asString(rec["body"])
					if text == "" {
						return jsonMsg(422, "body is required")
					}
					c := &comment{id: r.nextCommentID, body: text}
					r.nextCommentID++
					iss.comments = append(iss.comments, c)
					invalidateIssue(iss)
					return jsonBytes(201, commentJSON(r, iss, c))
				}
				return jsonMsg(404, "Not Found")
			}
			return jsonMsg(404, "Not Found")
		}

		// pulls
		if rest[0] == "pulls" {
			r, errResp := needRepo(owner, name)
			if errResp != nil {
				return *errResp
			}
			if len(rest) == 1 && method == "GET" {
				state := query.Get("state")
				if state == "" {
					state = "open"
				}
				items := append([]*pull(nil), r.pulls...)
				if state != "all" {
					filtered := items[:0]
					for _, p := range items {
						if p.state == state {
							filtered = append(filtered, p)
						}
					}
					items = filtered
				}
				// Sort by number descending.
				for i := 0; i < len(items); i++ {
					for j := i + 1; j < len(items); j++ {
						if items[j].number > items[i].number {
							items[i], items[j] = items[j], items[i]
						}
					}
				}
				page, perPage := pageParams(query)
				items = paginate(items, page, perPage)
				var w jw
				w.raw(`[`)
				for i, p := range items {
					if i > 0 {
						w.raw(`,`)
					}
					w.buf = append(w.buf, pullJSON(s, r, p)...)
				}
				w.raw(`]`)
				return jsonBytes(200, w.bytes())
			}
			if len(rest) == 1 && method == "POST" {
				parsed, ok := parseJSONBody(req.body)
				if !ok {
					return jsonMsg(400, "malformed JSON body")
				}
				if !isRecord(parsed) {
					return jsonMsg(422, "Validation Failed")
				}
				rec := parsed.(map[string]any)
				title, _ := asString(rec["title"])
				head, _ := asString(rec["head"])
				base, _ := asString(rec["base"])
				if title == "" || head == "" || base == "" {
					return jsonMsg(422, "title, head and base are required")
				}
				if idx := strings.LastIndex(head, ":"); idx >= 0 {
					head = head[idx+1:]
				}
				if idx := strings.LastIndex(base, ":"); idx >= 0 {
					base = base[idx+1:]
				}
				if _, ok := s.resolveRef(r, head); !ok {
					return jsonMsg(422, "Head branch '"+head+"' not found")
				}
				if _, ok := s.resolveRef(r, base); !ok {
					return jsonMsg(422, "Base branch '"+base+"' not found")
				}
				for _, p := range r.pulls {
					if p.state == "open" && p.headRef == head && p.baseRef == base {
						return jsonMsg(422, "A pull request already exists for these branches")
					}
				}
				bodyText, _ := asString(rec["body"])
				p := s.createPull(r, title, bodyText, head, base)
				if p == nil {
					return jsonMsg(422, "could not create the pull request")
				}
				return jsonBytes(201, pullJSON(s, r, p))
			}
			if len(rest) == 2 {
				number, err := strconv.Atoi(rest[1])
				if err != nil || number < 1 {
					return jsonMsg(404, "Not Found")
				}
				p := s.findPull(r, number)
				if p == nil {
					return jsonMsg(404, "Not Found")
				}
				if method == "GET" {
					return jsonBytes(200, pullJSON(s, r, p))
				}
				return jsonMsg(404, "Not Found")
			}
			if len(rest) == 3 && rest[2] == "files" && method == "GET" {
				number, err := strconv.Atoi(rest[1])
				if err != nil || number < 1 {
					return jsonMsg(404, "Not Found")
				}
				p := s.findPull(r, number)
				if p == nil {
					return jsonMsg(404, "Not Found")
				}
				baseTip, _ := s.resolveRef(r, p.baseRef)
				headTip, _ := s.resolveRef(r, p.headRef)
				var baseTree, headTree string
				if baseTip != "" {
					baseTree = parseCommit(r.objects[baseTip].data).tree
				}
				if headTip != "" {
					headTree = parseCommit(r.objects[headTip].data).tree
				}
				files := s.diffTrees(r, baseTree, headTree)
				var w jw
				w.raw(`[`)
				for i, file := range files {
					if i > 0 {
						w.raw(`,`)
					}
					w.raw(`{"filename":`)
					w.str(file.filename)
					w.raw(`,"status":`)
					w.str(file.status)
					w.raw(`,"additions":`)
					w.integer(file.additions)
					w.raw(`,"deletions":`)
					w.integer(file.deletions)
					w.raw(`,"changes":`)
					w.integer(file.additions + file.deletions)
					w.raw(`}`)
				}
				w.raw(`]`)
				return jsonBytes(200, w.bytes())
			}
			if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
				number, err := strconv.Atoi(rest[1])
				if err != nil || number < 1 {
					return jsonMsg(404, "Not Found")
				}
				p := s.findPull(r, number)
				if p == nil {
					return jsonMsg(404, "Not Found")
				}
				if p.state != "open" || p.merged {
					return jsonMsg(405, "Pull request is not mergeable")
				}
				if s.pullConflicts(r, p) {
					return jsonMsg(405, "Merge conflict")
				}
				mergeOid := s.mergePull(r, p)
				if mergeOid == "" {
					return jsonMsg(405, "Merge conflict")
				}
				var w jw
				w.raw(`{"merged":true,"sha":`)
				w.str(mergeOid)
				w.raw(`,"message":"Pull request successfully merged"}`)
				return jsonBytes(200, w.bytes())
			}
			return jsonMsg(404, "Not Found")
		}
	}

	// Nothing matched: GitHub's JSON 404.
	return jsonMsg(404, "Not Found")
}

// decodeSegment mirrors decodeURIComponent with a raw fallback.
func decodeSegment(part string) string {
	decoded, err := url.PathUnescape(part)
	if err != nil {
		return part
	}
	return decoded
}
