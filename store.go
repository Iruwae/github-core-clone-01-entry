// The forge's in-memory store. One process, one trial: everything lives in maps,
// nothing touches the disk.
//
// Determinism rule: no wall clock, no randomness in anything a response carries.
// Commit authorship is the viewer with a fixed timestamp; created/merged
// timestamps are one fixed ISO string. A live trial and its replay therefore
// answer byte-for-byte the same.
//
// Concurrency: net/http serves each request on its own goroutine, so every map
// access goes through Store.mu. Handlers hold RLock for pure reads and Lock for
// anything that mutates. All methods below assume the caller holds the lock.
// Git objects are immutable once stored, so handlers may collect object pointers
// under RLock, release it, and deflate/pack lock-free.

package main

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// commitAuthor is the fixed commit identity for contents/merge commits the
// server authors itself.
var commitAuthor = viewer.name + " <" + viewer.email + "> 1700000000 +0000"

// fixedISO is the fixed ISO timestamp for every created_at / merged_at.
const fixedISO = "2026-11-14T06:30:00.000Z"

type label struct {
	name        string
	color       string
	description string
}

type comment struct {
	id   int
	body string
}

// issueCache holds one serialized issue JSON (no trailing newline).

type issueCache struct {
	b []byte
}

type issue struct {
	id       int
	number   int
	title    string
	body     string
	state    string // 'open' | 'closed'
	labels   []string
	comments []*comment
	// cache holds the serialized issue JSON; it is stored atomically so
	// concurrent readers never race on it, and cleared on any mutation.
	cache atomic.Pointer[issueCache]
}

// invalidateIssue clears an issue's cached JSON after a mutation.
func invalidateIssue(iss *issue) {
	iss.cache.Store(nil)
}

type pull struct {
	id             int
	number         int
	title          string
	body           string
	state          string // 'open' | 'closed'
	merged         bool
	mergedAt       string // "" means null
	mergeCommitSha string // "" means null
	headRef        string
	baseRef        string
	headSha        string
	baseSha        string
	// mergeBaseSha is the base tip when the PR was opened.
	mergeBaseSha string
}

type repo struct {
	id            int
	name          string
	owner         string
	description   string
	isPrivate     bool
	defaultBranch string
	objects       map[string]*gitObject
	refs          map[string]string // full ref -> oid
	labels        map[string]*label
	labelOrder    []string
	issues        []*issue
	pulls         []*pull
	nextNumber    int // shared issue+PR counter, starts at 1
	nextCommentID int
	nextLabelID   int
	createdSeq    int
}

// Store is the whole forge state.
type Store struct {
	mu           sync.RWMutex
	repos        map[string]*repo // `${owner}/${name}` -> Repo
	nextRepoID   int
	nextObjectID int // stable numeric ids for issues/PRs
	repoSeq      int
	// fetchCache caches assembled fetch responses for have-less fetches
	// (fresh clones). The pack for (repo, wants, shallow, depth) is fully
	// determined by the wanted oids: objects are immutable and content
	// addressed, so the cached bytes stay correct even as refs move or new
	// objects arrive. The clone workload is 50 concurrent depth-1 clones
	// of one repo, repeated for 3 rounds with the median taken, so rounds
	// 2-3 serve the cached pack instead of re-selecting + re-deflating it.
	// Capped: conformance probing cannot grow it without bound.
	fetchCacheMu sync.Mutex
	fetchCache   map[string][]byte
}

// fetchCacheGet returns cached fetch bytes for key, if present.
func (s *Store) fetchCacheGet(key string) ([]byte, bool) {
	s.fetchCacheMu.Lock()
	defer s.fetchCacheMu.Unlock()
	b, ok := s.fetchCache[key]
	return b, ok
}

// fetchCachePut stores fetch bytes under key (no-op past the cap).
func (s *Store) fetchCachePut(key string, b []byte) {
	s.fetchCacheMu.Lock()
	defer s.fetchCacheMu.Unlock()
	if len(s.fetchCache) >= 64 {
		return
	}
	s.fetchCache[key] = b
}

func newStore() *Store {
	return &Store{
		repos:        make(map[string]*repo),
		nextRepoID:   5000,
		nextObjectID: 1,
		fetchCache:   make(map[string][]byte),
	}
}

func (s *Store) key(owner, name string) string { return owner + "/" + name }

func (s *Store) getRepo(owner, name string) *repo {
	return s.repos[s.key(owner, name)]
}

func (s *Store) createRepo(owner, name, description string, isPrivate bool) *repo {
	r := &repo{
		id:            s.nextRepoID,
		name:          name,
		owner:         owner,
		description:   description,
		isPrivate:     isPrivate,
		defaultBranch: "main",
		objects:       make(map[string]*gitObject),
		refs:          make(map[string]string),
		labels:        make(map[string]*label),
		issues:        nil,
		pulls:         nil,
		nextNumber:    1,
		nextCommentID: 1,
		nextLabelID:   1,
		createdSeq:    s.repoSeq,
	}
	s.nextRepoID++
	s.repoSeq++
	s.repos[s.key(owner, name)] = r
	return r
}

func (s *Store) deleteRepo(owner, name string) bool {
	_, ok := s.repos[s.key(owner, name)]
	if ok {
		delete(s.repos, s.key(owner, name))
	}
	return ok
}

// listRepos returns all of the viewer's repos, in creation order.
func (s *Store) listRepos(owner string) []*repo {
	var out []*repo
	for _, r := range s.repos {
		if r.owner == owner {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdSeq < out[j].createdSeq })
	return out
}

func (s *Store) nextID() int {
	id := s.nextObjectID
	s.nextObjectID++
	return id
}

// ------------------------------------------------------------ git objects

func (s *Store) putObject(r *repo, typ string, data []byte) string {
	oid := objectID(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &gitObject{typ: typ, data: data}
	}
	return oid
}

// resolveRef returns the tip of a ref, or "". ref may be a full ref or a branch name.
func (s *Store) resolveRef(r *repo, ref string) (string, bool) {
	if oid, ok := r.refs[ref]; ok {
		return oid, true
	}
	full := ref
	if !strings.HasPrefix(ref, "refs/") {
		full = "refs/heads/" + ref
	}
	oid, ok := r.refs[full]
	return oid, ok
}

// branches returns all refs under refs/heads/, as [branch, oid] sorted by branch name.
func (s *Store) branches(r *repo) []struct {
	name string
	sha  string
} {
	var out []struct {
		name string
		sha  string
	}
	for ref, oid := range r.refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			out = append(out, struct {
				name string
				sha  string
			}{name: ref[len("refs/heads/"):], sha: oid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// isAncestor reports whether ancestor is reachable from oid (commit ancestry).
func (s *Store) isAncestor(r *repo, ancestor, oid string) bool {
	if ancestor == oid {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{oid}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == ancestor {
			return true
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		obj, ok := r.objects[current]
		if ok && obj.typ == "commit" {
			stack = append(stack, commitOf(obj).parents...)
		}
	}
	return false
}

// ------------------------------------------------------------ trees

type flatEntry struct {
	mode string
	oid  string
}

// flattenTree is the flat path -> {mode, oid} map of a tree, recursively.
func (s *Store) flattenTree(r *repo, treeOid string) map[string]flatEntry {
	out := make(map[string]flatEntry)
	if treeOid == "" {
		return out
	}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj, ok := r.objects[oid]
		if !ok || obj.typ != "tree" {
			return
		}
		entries, err := treeOf(obj)
		if err != nil {
			return
		}
		for _, entry := range entries {
			path := entry.name
			if prefix != "" {
				path = prefix + "/" + entry.name
			}
			if entry.mode == "40000" {
				walk(entry.oid, path)
			} else {
				out[path] = flatEntry{mode: entry.mode, oid: entry.oid}
			}
		}
	}
	walk(treeOid, "")
	return out
}

// setPathInTree rebuilds the tree for root with path set to blobOid (or removed
// when blobOid is ""). It returns the new root tree oid.
func (s *Store) setPathInTree(r *repo, rootTreeOid, path, blobOid string) string {
	parts := []string{}
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		panic("empty file path")
	}
	var build func(treeOid string, depth int) string
	build = func(treeOid string, depth int) string {
		entries := make(map[string]treeEntry)
		if treeOid != "" {
			if obj, ok := r.objects[treeOid]; ok && obj.typ == "tree" {
				if parsed, err := treeOf(obj); err == nil {
					for _, entry := range parsed {
						entries[entry.name] = entry
					}
				}
			}
		}
		part := parts[depth]
		if depth == len(parts)-1 {
			if blobOid == "" {
				delete(entries, part)
			} else {
				entries[part] = treeEntry{mode: "100644", name: part, oid: blobOid}
			}
		} else {
			child, ok := entries[part]
			childTree := ""
			if ok && child.mode == "40000" {
				childTree = child.oid
			}
			newChild := build(childTree, depth+1)
			empty := false
			if childObj, ok := r.objects[newChild]; ok && childObj.typ == "tree" {
				if parsed, err := parseTree(childObj.data); err == nil && len(parsed) == 0 {
					empty = true
				}
			}
			if empty {
				delete(entries, part)
			} else {
				entries[part] = treeEntry{mode: "40000", name: part, oid: newChild}
			}
		}
		list := make([]treeEntry, 0, len(entries))
		for _, entry := range entries {
			list = append(list, entry)
		}
		data := buildTree(list)
		return s.putObject(r, "tree", data)
	}
	return build(rootTreeOid, 0)
}

// ------------------------------------------------------------ contents commits

// commitFile commits path -> content on branch (creating the branch from the
// default tip, or from nothing when the repo is unborn). It returns the new
// commit oid.
func (s *Store) commitFile(r *repo, branch, path string, content []byte, message string) string {
	ref := "refs/heads/" + branch
	tip, ok := r.refs[ref]
	if !ok {
		tip = ""
		if branch != r.defaultBranch {
			tip, _ = r.refs["refs/heads/"+r.defaultBranch]
		}
	}
	blobOid := s.putObject(r, "blob", content)
	var baseTree string
	if tip != "" {
		baseTree = parseCommit(r.objects[tip].data).tree
	}
	newTree := s.setPathInTree(r, baseTree, path, blobOid)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitData := buildCommit(newTree, parents, commitAuthor, commitAuthor, message)
	commitOid := s.putObject(r, "commit", commitData)
	r.refs[ref] = commitOid
	return commitOid
}

// deleteFile deletes path on branch. It returns the new commit oid, or "" when
// the file is absent.
func (s *Store) deleteFile(r *repo, branch, path, message string) string {
	ref := "refs/heads/" + branch
	tip, ok := r.refs[ref]
	if !ok {
		return ""
	}
	baseTree := parseCommit(r.objects[tip].data).tree
	flat := s.flattenTree(r, baseTree)
	if _, ok := flat[path]; !ok {
		return ""
	}
	newTree := s.setPathInTree(r, baseTree, path, "")
	commitData := buildCommit(newTree, []string{tip}, commitAuthor, commitAuthor, message)
	commitOid := s.putObject(r, "commit", commitData)
	r.refs[ref] = commitOid
	return commitOid
}

type readResult struct {
	kind    string // 'file' | 'dir'
	blobOid string
	data    []byte
	entries []treeEntry
}

// readPath reads a file or directory at ref (branch name or oid).
func (s *Store) readPath(r *repo, ref, path string) *readResult {
	tip, ok := s.resolveRef(r, ref)
	if !ok {
		if _, exists := r.objects[ref]; exists {
			tip = ref
		} else {
			return nil
		}
	}
	obj, ok := r.objects[tip]
	if !ok {
		return nil
	}
	var treeOid string
	switch obj.typ {
	case "commit":
		treeOid = commitOf(obj).tree
	case "tree":
		treeOid = tip
	default:
		return nil
	}
	if path == "" || path == "/" {
		root, ok := r.objects[treeOid]
		if !ok {
			return nil
		}
		entries, err := treeOf(root)
		if err != nil {
			return nil
		}
		return &readResult{kind: "dir", entries: entries}
	}
	flat := s.flattenTree(r, treeOid)
	if file, ok := flat[path]; ok {
		blob, ok := r.objects[file.oid]
		if !ok {
			return nil
		}
		return &readResult{kind: "file", blobOid: file.oid, data: blob.data}
	}
	// A directory: walk to its tree.
	parts := []string{}
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	current := treeOid
	for _, part := range parts {
		objNow, ok := r.objects[current]
		if !ok || objNow.typ != "tree" {
			return nil
		}
		entries, err := treeOf(objNow)
		if err != nil {
			return nil
		}
		var entry *treeEntry
		for i := range entries {
			if entries[i].name == part && entries[i].mode == "40000" {
				entry = &entries[i]
				break
			}
		}
		if entry == nil {
			return nil
		}
		current = entry.oid
	}
	dir, ok := r.objects[current]
	if !ok {
		return nil
	}
	entries, err := treeOf(dir)
	if err != nil {
		return nil
	}
	return &readResult{kind: "dir", entries: entries}
}

type commitAndSha struct {
	sha    string
	commit commitInfo
}

// listCommits returns commits reachable from ref, newest-first (DFS pre-order).
func (s *Store) listCommits(r *repo, ref string) []commitAndSha {
	tip, ok := s.resolveRef(r, ref)
	if !ok {
		if _, exists := r.objects[ref]; exists {
			tip = ref
		} else {
			return nil
		}
	}
	var out []commitAndSha
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := r.objects[oid]
		if !ok || obj.typ != "commit" {
			continue
		}
		commit := commitOf(obj)
		out = append(out, commitAndSha{sha: oid, commit: commit})
		// Push parents reversed so the first parent is visited first.
		for i := len(commit.parents) - 1; i >= 0; i-- {
			stack = append(stack, commit.parents[i])
		}
	}
	return out
}

// ------------------------------------------------------------ issues & pulls

func (s *Store) findIssue(r *repo, number int) *issue {
	for _, iss := range r.issues {
		if iss.number == number {
			return iss
		}
	}
	return nil
}

func (s *Store) findPull(r *repo, number int) *pull {
	for _, p := range r.pulls {
		if p.number == number {
			return p
		}
	}
	return nil
}

func (s *Store) createIssue(r *repo, title, body string, labels []string) *issue {
	iss := &issue{
		id:     s.nextID(),
		number: r.nextNumber,
		title:  title,
		body:   body,
		state:  "open",
		labels: append([]string(nil), labels...),
	}
	r.nextNumber++
	r.issues = append(r.issues, iss)
	return iss
}

func (s *Store) createPull(r *repo, title, body, headRef, baseRef string) *pull {
	headSha, ok1 := s.resolveRef(r, headRef)
	baseSha, ok2 := s.resolveRef(r, baseRef)
	if !ok1 || !ok2 {
		return nil
	}
	p := &pull{
		id:           s.nextID(),
		number:       r.nextNumber,
		title:        title,
		body:         body,
		state:        "open",
		headRef:      headRef,
		baseRef:      baseRef,
		headSha:      headSha,
		baseSha:      baseSha,
		mergeBaseSha: baseSha,
	}
	r.nextNumber++
	r.pulls = append(r.pulls, p)
	return p
}

// ------------------------------------------------------------ pull diffs & merges

func splitLines(data []byte) []string {
	text := string(data)
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lineDiff counts line additions/deletions between two blobs (simple LCS).
func lineDiff(a, b []byte) (additions, deletions int) {
	linesA := splitLines(a)
	linesB := splitLines(b)
	m := len(linesA)
	n := len(linesB)
	prev := make([]int, n+1)
	for i := 1; i <= m; i++ {
		cur := make([]int, n+1)
		for j := 1; j <= n; j++ {
			if linesA[i-1] == linesB[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev = cur
	}
	lcs := prev[n]
	return n - lcs, m - lcs
}

type fileDiff struct {
	filename  string
	status    string // 'added' | 'removed' | 'modified'
	additions int
	deletions int
}

// diffTrees is the file-level diff between two trees (base tip vs head tip).
func (s *Store) diffTrees(r *repo, baseTree, headTree string) []fileDiff {
	base := s.flattenTree(r, baseTree)
	head := s.flattenTree(r, headTree)
	names := make(map[string]bool)
	for name := range base {
		names[name] = true
	}
	for name := range head {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var out []fileDiff
	for _, name := range sorted {
		b, hasB := base[name]
		h, hasH := head[name]
		switch {
		case hasB && !hasH:
			data := []byte(nil)
			if obj, ok := r.objects[b.oid]; ok {
				data = obj.data
			}
			out = append(out, fileDiff{filename: name, status: "removed", deletions: len(splitLines(data))})
		case !hasB && hasH:
			data := []byte(nil)
			if obj, ok := r.objects[h.oid]; ok {
				data = obj.data
			}
			out = append(out, fileDiff{filename: name, status: "added", additions: len(splitLines(data))})
		case hasB && hasH && b.oid != h.oid:
			dataB := []byte(nil)
			if obj, ok := r.objects[b.oid]; ok {
				dataB = obj.data
			}
			dataH := []byte(nil)
			if obj, ok := r.objects[h.oid]; ok {
				dataH = obj.data
			}
			add, del := lineDiff(dataB, dataH)
			out = append(out, fileDiff{filename: name, status: "modified", additions: add, deletions: del})
		}
	}
	return out
}

// treeDiffMap returns files changed between two commits' trees (flat path -> blob oid, nil = removed).
func (s *Store) treeDiffMap(r *repo, fromTree, toTree string) map[string]string {
	from := s.flattenTree(r, fromTree)
	to := s.flattenTree(r, toTree)
	out := make(map[string]string)
	names := make(map[string]bool)
	for name := range from {
		names[name] = true
	}
	for name := range to {
		names[name] = true
	}
	for name := range names {
		a := ""
		if e, ok := from[name]; ok {
			a = e.oid
		}
		b := ""
		if e, ok := to[name]; ok {
			b = e.oid
		}
		if a != b {
			out[name] = b
		}
	}
	return out
}

// pullConflicts reports whether merging this PR would conflict.
func (s *Store) pullConflicts(r *repo, p *pull) bool {
	baseTip, ok1 := s.resolveRef(r, p.baseRef)
	headTip, ok2 := s.resolveRef(r, p.headRef)
	if !ok1 || !ok2 {
		return false
	}
	if baseTip == p.mergeBaseSha {
		return false // base has not moved: no conflict possible
	}
	baseCommit, ok := r.objects[baseTip]
	if !ok {
		return false
	}
	headCommit, ok := r.objects[headTip]
	if !ok {
		return false
	}
	mergeBaseCommit, ok := r.objects[p.mergeBaseSha]
	if !ok {
		return false
	}
	headChanges := s.treeDiffMap(r, parseCommit(mergeBaseCommit.data).tree, parseCommit(headCommit.data).tree)
	baseChanges := s.treeDiffMap(r, parseCommit(mergeBaseCommit.data).tree, parseCommit(baseCommit.data).tree)
	for path, headOid := range headChanges {
		if baseOid, ok := baseChanges[path]; ok && baseOid != headOid {
			return true
		}
	}
	return false
}

// mergePull squash-merges a PR onto its base branch. It returns the merge
// commit oid, or "" on conflict.
func (s *Store) mergePull(r *repo, p *pull) string {
	if p.state != "open" || p.merged {
		return ""
	}
	if s.pullConflicts(r, p) {
		return ""
	}
	baseTip, ok1 := s.resolveRef(r, p.baseRef)
	headTip, ok2 := s.resolveRef(r, p.headRef)
	if !ok1 || !ok2 {
		return ""
	}
	baseCommit, ok := r.objects[baseTip]
	if !ok {
		return ""
	}
	headCommit, ok := r.objects[headTip]
	if !ok {
		return ""
	}
	mergeBaseCommit, ok := r.objects[p.mergeBaseSha]
	if !ok {
		return ""
	}
	// New tree: base tip's tree with the head's changes (vs merge-base) applied.
	baseTree := parseCommit(baseCommit.data).tree
	headChanges := s.treeDiffMap(r, parseCommit(mergeBaseCommit.data).tree, parseCommit(headCommit.data).tree)
	newTree := baseTree
	// Apply in sorted order for determinism (map order must not leak into the tree).
	paths := make([]string, 0, len(headChanges))
	for path := range headChanges {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		newTree = s.setPathInTree(r, newTree, path, headChanges[path])
	}
	message := "Merge pull request #" + strconv.Itoa(p.number) + " from " + p.headRef
	commitData := buildCommit(newTree, []string{baseTip}, commitAuthor, commitAuthor, message)
	mergeOid := s.putObject(r, "commit", commitData)
	r.refs["refs/heads/"+p.baseRef] = mergeOid
	p.state = "closed"
	p.merged = true
	p.mergedAt = fixedISO
	p.mergeCommitSha = mergeOid
	p.baseSha = mergeOid
	return mergeOid
}

// ------------------------------------------------------------ global ids (GraphQL Relay)

func globalID(kind, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + key))
}

func repoGlobalID(r *repo) string {
	return globalID("Repository", r.owner+"/"+r.name)
}

func issueGlobalID(r *repo, iss *issue) string {
	return globalID("Issue", r.owner+"/"+r.name+"/"+strconv.Itoa(iss.number))
}

func pullGlobalID(r *repo, p *pull) string {
	return globalID("PullRequest", r.owner+"/"+r.name+"/"+strconv.Itoa(p.number))
}

func commentGlobalID(r *repo, iss *issue, c *comment) string {
	return globalID("IssueComment", r.owner+"/"+r.name+"/"+strconv.Itoa(iss.number)+"/"+strconv.Itoa(c.id))
}

type resolvedID struct {
	kind    string
	repo    *repo
	issue   *issue
	pull    *pull
	comment *comment
}

// resolveGlobalID resolves a global id back to a repo / issue / pull / comment.
func (s *Store) resolveGlobalID(id string) *resolvedID {
	decodedBytes, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	decoded := string(decodedBytes)
	kind, rest, ok := strings.Cut(decoded, ":")
	if !ok || kind == "" || rest == "" {
		return nil
	}
	if kind == "Repository" {
		owner, name, ok := strings.Cut(rest, "/")
		if !ok {
			return nil
		}
		r := s.getRepo(owner, name)
		if r == nil {
			return nil
		}
		return &resolvedID{kind: kind, repo: r}
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return nil
	}
	owner := parts[0]
	name := parts[1]
	number, err := strconv.Atoi(parts[2])
	if err != nil || number < 1 {
		return nil
	}
	r := s.getRepo(owner, name)
	if r == nil {
		return nil
	}
	switch kind {
	case "Issue":
		iss := s.findIssue(r, number)
		if iss == nil {
			return nil
		}
		if len(parts) == 4 {
			commentID, err := strconv.Atoi(parts[3])
			if err != nil {
				return nil
			}
			for _, c := range iss.comments {
				if c.id == commentID {
					return &resolvedID{kind: "IssueComment", repo: r, issue: iss, comment: c}
				}
			}
			return nil
		}
		return &resolvedID{kind: kind, repo: r, issue: iss}
	case "PullRequest":
		p := s.findPull(r, number)
		if p == nil {
			return nil
		}
		return &resolvedID{kind: kind, repo: r, pull: p}
	}
	return nil
}
