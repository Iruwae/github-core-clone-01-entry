// REST v3 (api.github.com shape): repos, refs, branches, commits, contents
// (backed by the same git object store as smart HTTP), labels, issues, comments
// and pull requests. Every reply is JSON with a non-empty X-GitHub-Request-Id
// (added by server.go); 401/404/422 bodies carry a string message; a malformed
// JSON body is a 400.
//
// Serializers build JSON by hand (field order matches the reference, output is
// deterministic) instead of encoding/json over maps, which would sort keys and
// cost reflection per call. The issue serializer is the benchmark hot path:
// GET issue reuses the cached bytes until the issue mutates.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------- JSON writer

// jw is a minimal JSON object/array writer over a byte buffer.
type jw struct {
	buf []byte
}

func (w *jw) raw(s string)      { w.buf = append(w.buf, s...) }
func (w *jw) byte(c byte)       { w.buf = append(w.buf, c) }
// str appends s as a JSON string. Fast path: when s is valid UTF-8 and
// contains no byte json.Marshal would escape, the quoted string is appended
// directly — byte-identical to Marshal's output, without its allocation.
// Marshal escapes <, >, & (HTML), control bytes, '"' and '\', and U+2028/29;
// anything else falls back to Marshal.
func (w *jw) str(s string) {
	if utf8.ValidString(s) && jsonSafeString(s) {
		w.byte('"')
		w.raw(s)
		w.byte('"')
		return
	}
	b, _ := json.Marshal(s)
	w.buf = append(w.buf, b...)
}

// jsonSafeString reports whether s contains no byte json.Marshal would escape.
func jsonSafeString(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			return false
		}
		if c == 0xE2 && i+2 < len(s) && s[i+1] == 0x80 && (s[i+2] == 0xA8 || s[i+2] == 0xA9) {
			return false // U+2028/U+2029
		}
	}
	return true
}
func (w *jw) integer(n int)     { w.buf = strconv.AppendInt(w.buf, int64(n), 10) }
func (w *jw) boolean(b bool)    { if b { w.buf = append(w.buf, "true"...); } else { w.buf = append(w.buf, "false"...); } }
func (w *jw) null()             { w.buf = append(w.buf, "null"...)}
func (w *jw) bytes() []byte     { return w.buf }

// ---------------------------------------------------------------- helpers

func jsonMsg(status int, text string) response {
	var w jw
	w.raw(`{"message":`)
	w.str(text)
	w.raw(`}`)
	return jsonBytes(status, w.bytes())
}

func parseJSONBody(body []byte) (any, bool) {
	if len(body) == 0 {
		return map[string]any{}, true
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, false
	}
	return value, true
}

// parseIssueCreate decodes an issue-create body without the map[string]any
// boxing of parseJSONBody: keys are exact-case (like the map path) and each
// value is decoded from its raw JSON. It returns ok=false for any shape it
// cannot represent exactly, and the caller falls back to parseJSONBody, so
// every status code and message stays identical to the generic path.
// A null top-level body is also a fallback (the generic path answers 422
// "Validation Failed", not 422 "title is required").
func parseIssueCreate(body []byte) (title, bodyText string, labels []string, ok bool) {
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(body, &rec); err != nil || rec == nil {
		return "", "", nil, false
	}
	if raw, present := rec["title"]; present {
		var t string
		if err := json.Unmarshal(raw, &t); err != nil {
			return "", "", nil, false
		}
		title = t
	}
	if raw, present := rec["body"]; present {
		var b string
		if err := json.Unmarshal(raw, &b); err != nil {
			return "", "", nil, false
		}
		bodyText = b
	}
	if raw, present := rec["labels"]; present {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return "", "", nil, false
		}
		for _, item := range items {
			if len(item) == 0 || item[0] == 'n' { // null: skipped, as in the generic path
				continue
			}
			var s string
			if err := json.Unmarshal(item, &s); err == nil {
				labels = append(labels, s)
				continue
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(item, &obj); err == nil {
				if nameRaw, hasName := obj["name"]; hasName {
					var n string
					if err := json.Unmarshal(nameRaw, &n); err == nil {
						labels = append(labels, n)
					}
				}
			}
			// Numbers, booleans and arrays are skipped, as in the generic path.
		}
	}
	return title, bodyText, labels, true
}

func isRecord(value any) bool {
	_, ok := value.(map[string]any)
	return ok
}

func asString(value any) (string, bool) {
	s, ok := value.(string)
	return s, ok
}

// parseAuthorLine parses a `Name <email> <unix> <tz>` author line.
func parseAuthorLine(line string) (name, email, date string) {
	// ^(.*?) <([^>]*)> (\d+) ([+-]\d{4})$
	lt := strings.Index(line, " <")
	if lt < 0 {
		return line, "", fixedISO
	}
	gt := strings.Index(line[lt+2:], ">")
	if gt < 0 {
		return line, "", fixedISO
	}
	rest := line[lt+2+gt+1:]
	rest = strings.TrimPrefix(rest, " ")
	fields := strings.SplitN(rest, " ", 2)
	if len(fields) != 2 {
		return line, "", fixedISO
	}
	secs, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return line, "", fixedISO
	}
	tz := fields[1]
	if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return line, "", fixedISO
	}
	for _, c := range tz[1:] {
		if c < '0' || c > '9' {
			return line, "", fixedISO
		}
	}
	name = line[:lt]
	email = line[lt+2 : lt+2+gt]
	date = time.Unix(secs, 0).UTC().Format("2006-01-02T15:04:05.000Z")
	return name, email, date
}

// encodeURIComponent mirrors JavaScript's encodeURIComponent for the label URL.
func encodeURIComponent(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' || c == '\'' || c == '(' || c == ')' {
			out.WriteByte(c)
		} else {
			const hexDigits = "0123456789ABCDEF"
			out.WriteByte('%')
			out.WriteByte(hexDigits[c>>4])
			out.WriteByte(hexDigits[c&0x0f])
		}
	}
	return out.String()
}

// ---------------------------------------------------------------- serializers

func repoJSON(r *repo) []byte {
	open := 0
	for _, iss := range r.issues {
		if iss.state == "open" {
			open++
		}
	}
	var w jw
	w.raw(`{"id":`)
	w.integer(r.id)
	w.raw(`,"node_id":`)
	w.str(base64.StdEncoding.EncodeToString([]byte("Repository:" + r.owner + "/" + r.name)))
	w.raw(`,"name":`)
	w.str(r.name)
	w.raw(`,"full_name":`)
	w.str(r.owner + "/" + r.name)
	w.raw(`,"private":`)
	w.boolean(r.isPrivate)
	w.raw(`,"owner":{"login":`)
	w.str(r.owner)
	w.raw(`,"id":`)
	w.integer(viewer.id)
	w.raw(`,"type":"User"},"description":`)
	w.str(r.description)
	w.raw(`,"fork":false,"created_at":"`)
	w.raw(fixedISO)
	w.raw(`","updated_at":"`)
	w.raw(fixedISO)
	w.raw(`","pushed_at":"`)
	w.raw(fixedISO)
	w.raw(`","homepage":null,"size":0,"stargazers_count":0,"watchers_count":0,"language":null,"has_issues":true,"has_projects":true,"has_downloads":true,"has_wiki":true,"has_pages":false,"forks_count":0,"open_issues_count":`)
	w.integer(open)
	w.raw(`,"license":null,"allow_forking":true,"is_template":false,"topics":[],"visibility":`)
	w.str(map[bool]string{true: "private", false: "public"}[r.isPrivate])
	w.raw(`,"default_branch":`)
	w.str(r.defaultBranch)
	w.raw(`}`)
	return w.bytes()
}

func userJSON() []byte {
	var w jw
	w.raw(`{"login":`)
	w.str(viewer.login)
	w.raw(`,"id":`)
	w.integer(viewer.id)
	w.raw(`,"node_id":`)
	w.str(base64.StdEncoding.EncodeToString([]byte("User:" + viewer.login)))
	w.raw(`,"avatar_url":"","type":"User","site_admin":false,"name":`)
	w.str(viewer.name)
	w.raw(`,"company":null,"blog":"","location":"","email":`)
	w.str(viewer.email)
	w.raw(`,"hireable":null,"bio":null,"public_repos":0,"public_gists":0,"followers":0,"following":0,"created_at":"`)
	w.raw(fixedISO)
	w.raw(`","updated_at":"`)
	w.raw(fixedISO)
	w.raw(`"}`)
	return w.bytes()
}

func labelJSON(r *repo, name string) []byte {
	lbl, ok := r.labels[name]
	if !ok {
		return nil
	}
	index := 0
	for i, n := range r.labelOrder {
		if n == name {
			index = i
			break
		}
	}
	var w jw
	w.raw(`{"id":`)
	w.integer(r.id*1000 + index + 1)
	w.raw(`,"node_id":`)
	w.str(base64.StdEncoding.EncodeToString([]byte("Label:" + r.owner + "/" + r.name + "/" + name)))
	w.raw(`,"url":`)
	w.str("/api/v3/repos/" + r.owner + "/" + r.name + "/labels/" + encodeURIComponent(name))
	w.raw(`,"name":`)
	w.str(lbl.name)
	w.raw(`,"color":`)
	w.str(lbl.color)
	w.raw(`,"description":`)
	w.str(lbl.description)
	w.raw(`,"default":false}`)
	return w.bytes()
}

// issueJSONBytes serializes one issue. With asPull nil it reuses the cached
// bytes until the issue mutates (the read workload's 2,000 GETs of one issue
// are the same bytes every time).
func issueJSONBytes(r *repo, iss *issue, asPull *pull) []byte {
	if asPull == nil {
		if cached := iss.cache.Load(); cached != nil {
			return cached.b
		}
	}
	built := buildIssueJSON(r, iss, asPull)
	if asPull == nil {
		iss.cache.Store(&issueCache{b: built})
	}
	return built
}

func buildIssueJSON(r *repo, iss *issue, asPull *pull) []byte {
	var w jw
	w.raw(`{"id":`)
	w.integer(iss.id)
	w.raw(`,"node_id":`)
	w.str(base64.StdEncoding.EncodeToString([]byte("Issue:" + r.owner + "/" + r.name + "/" + strconv.Itoa(iss.number))))
	w.raw(`,"number":`)
	w.integer(iss.number)
	w.raw(`,"title":`)
	w.str(iss.title)
	w.raw(`,"body":`)
	w.str(iss.body)
	w.raw(`,"state":`)
	w.str(iss.state)
	w.raw(`,"locked":false,"labels":[`)
	first := true
	for _, name := range iss.labels {
		lbl := labelJSON(r, name)
		if lbl == nil {
			continue
		}
		if !first {
			w.raw(`,`)
		}
		first = false
		w.buf = append(w.buf, lbl...)
	}
	w.raw(`],"user":{"login":`)
	w.str(viewer.login)
	w.raw(`,"id":`)
	w.integer(viewer.id)
	w.raw(`,"type":"User"},"assignee":null,"assignees":[],"milestone":null,"comments":`)
	w.integer(len(iss.comments))
	w.raw(`,"created_at":"`)
	w.raw(fixedISO)
	w.raw(`","updated_at":"`)
	w.raw(fixedISO)
	w.raw(`","closed_at":`)
	if iss.state == "closed" {
		w.str(fixedISO)
	} else {
		w.null()
	}
	w.raw(`,"author_association":"OWNER"`)
	if asPull != nil {
		w.raw(`,"pull_request":{"url":`)
		w.str("/api/v3/repos/" + r.owner + "/" + r.name + "/pulls/" + strconv.Itoa(asPull.number))
		w.raw(`,"html_url":"","diff_url":"","patch_url":""}`)
	}
	w.raw(`}`)
	return w.bytes()
}

func pullJSON(s *Store, r *repo, p *pull) []byte {
	mergeable := false
	if !p.merged && p.state == "open" {
		mergeable = !s.pullConflicts(r, p)
	}
	var w jw
	w.raw(`{"id":`)
	w.integer(p.id)
	w.raw(`,"node_id":`)
	w.str(base64.StdEncoding.EncodeToString([]byte("PullRequest:" + r.owner + "/" + r.name + "/" + strconv.Itoa(p.number))))
	w.raw(`,"number":`)
	w.integer(p.number)
	w.raw(`,"state":`)
	w.str(p.state)
	w.raw(`,"title":`)
	w.str(p.title)
	w.raw(`,"body":`)
	w.str(p.body)
	w.raw(`,"user":{"login":`)
	w.str(viewer.login)
	w.raw(`,"id":`)
	w.integer(viewer.id)
	w.raw(`,"type":"User"},"head":{"ref":`)
	w.str(p.headRef)
	w.raw(`,"sha":`)
	w.str(p.headSha)
	w.raw(`,"label":`)
	w.str(r.owner + ":" + p.headRef)
	w.raw(`,"user":{"login":`)
	w.str(r.owner)
	w.raw(`,"id":`)
	w.integer(viewer.id)
	w.raw(`,"type":"User"},"repo":`)
	w.buf = append(w.buf, repoJSON(r)...)
	w.raw(`},"base":{"ref":`)
	w.str(p.baseRef)
	w.raw(`,"sha":`)
	w.str(p.baseSha)
	w.raw(`,"label":`)
	w.str(r.owner + ":" + p.baseRef)
	w.raw(`,"user":{"login":`)
	w.str(r.owner)
	w.raw(`,"id":`)
	w.integer(viewer.id)
	w.raw(`,"type":"User"},"repo":`)
	w.buf = append(w.buf, repoJSON(r)...)
	w.raw(`},"merged":`)
	w.boolean(p.merged)
	w.raw(`,"mergeable":`)
	w.boolean(mergeable)
	w.raw(`,"mergeable_state":`)
	if p.merged || mergeable {
		w.str("clean")
	} else {
		w.str("dirty")
	}
	w.raw(`,"merged_at":`)
	if p.mergedAt != "" {
		w.str(p.mergedAt)
	} else {
		w.null()
	}
	w.raw(`,"merge_commit_sha":`)
	if p.mergeCommitSha != "" {
		w.str(p.mergeCommitSha)
	} else {
		w.null()
	}
	w.raw(`,"draft":false,"created_at":"`)
	w.raw(fixedISO)
	w.raw(`","updated_at":"`)
	w.raw(fixedISO)
	w.raw(`","closed_at":`)
	if p.state == "closed" {
		w.str(fixedISO)
	} else {
		w.null()
	}
	w.raw(`}`)
	return w.bytes()
}

func commentJSON(r *repo, iss *issue, c *comment) []byte {
	var w jw
	w.raw(`{"id":`)
	w.integer(c.id)
	w.raw(`,"node_id":`)
	w.str(base64.StdEncoding.EncodeToString([]byte("IssueComment:" + r.owner + "/" + r.name + "/" + strconv.Itoa(iss.number) + "/" + strconv.Itoa(c.id))))
	w.raw(`,"body":`)
	w.str(c.body)
	w.raw(`,"user":{"login":`)
	w.str(viewer.login)
	w.raw(`,"id":`)
	w.integer(viewer.id)
	w.raw(`,"type":"User"},"created_at":"`)
	w.raw(fixedISO)
	w.raw(`","updated_at":"`)
	w.raw(fixedISO)
	w.raw(`"}`)
	return w.bytes()
}

// ---------------------------------------------------------------- pagination

func pageParams(query url.Values) (page, perPage int) {
	// Mirrors the reference: page = max(1, Number(page) || 1),
	// perPage = min(100, max(1, Number(per_page) || 30)).
	page = 1
	if v := query.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			if n < 1 {
				n = 1
			}
			page = n
		}
	}
	perPage = 30
	if v := query.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			if n < 1 {
				n = 1
			}
			if n > 100 {
				n = 100
			}
			perPage = n
		}
	}
	return page, perPage
}

func linkHeader(origin, path string, query url.Values, page, perPage, total int) string {
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if lastPage <= 1 && page == 1 {
		return ""
	}
	build := func(p int) string {
		q := url.Values{}
		for k, vs := range query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("page", strconv.Itoa(p))
		q.Set("per_page", strconv.Itoa(perPage))
		return origin + path + "?" + q.Encode()
	}
	var parts []string
	if page < lastPage {
		parts = append(parts, "<"+build(page+1)+`>; rel="next"`)
	}
	if page > 1 {
		parts = append(parts, "<"+build(page-1)+`>; rel="prev"`)
	}
	parts = append(parts, "<"+build(1)+`>; rel="first"`)
	parts = append(parts, "<"+build(lastPage)+`>; rel="last"`)
	return strings.Join(parts, ", ")
}

func paginate[T any](items []T, page, perPage int) []T {
	start := (page - 1) * perPage
	if start >= len(items) {
		return nil
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}
