// Git smart HTTP: info/refs advertisements (v2 for upload-pack when the client
// asks, v0 otherwise and always for receive-pack), v2 ls-refs / fetch, v0 fetch,
// and receive-pack pushes with per-ref validation and sideband report-status.
//
// Content types matter: advertisements are application/x-<service>-advertisement,
// POST results are application/x-<service>-result (the client's post() enforces them).

package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const agentString = "github-forge/1.0.0"

// peeledCommit returns the peeled commit of an annotated tag ref, or "".
func peeledCommit(r *repo, ref string) string {
	oid, ok := r.refs[ref]
	if !ok {
		return ""
	}
	for i := 0; i < 5 && oid != ""; i++ {
		obj, ok := r.objects[oid]
		if !ok {
			return ""
		}
		if obj.typ == "commit" {
			return oid
		}
		if obj.typ == "tag" {
			oid = parseTag(obj.data).object
		} else {
			return ""
		}
	}
	return ""
}

type refEntry struct {
	name   string
	oid    string
	peeled string
}

// sortedRefs returns all refs sorted by name, each with its peeled commit when
// it is an annotated tag.
func sortedRefs(r *repo) []refEntry {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]refEntry, 0, len(names))
	for _, name := range names {
		oid := r.refs[name]
		peeled := ""
		if obj, ok := r.objects[oid]; ok && obj.typ == "tag" {
			peeled = peeledCommit(r, name)
		}
		out = append(out, refEntry{name: name, oid: oid, peeled: peeled})
	}
	return out
}

func v0Capabilities(service string) string {
	common := "symref=HEAD:refs/heads/main agent=" + agentString
	if service == "git-receive-pack" {
		return common + " report-status report-status-v2 side-band-64k delete-refs ofs-delta"
	}
	return common + " side-band-64k shallow ofs-delta"
}

// v0Advertisement builds a v0 ref advertisement: service preamble, then HEAD +
// refs (+ peeled), then flush.
func v0Advertisement(r *repo, service string) []byte {
	refs := sortedRefs(r)
	caps := v0Capabilities(service)
	var items []any
	items = append(items, "# service="+service+"\n", "flush")
	if len(refs) == 0 {
		items = append(items, zeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		mainTip := r.refs["refs/heads/main"]
		if mainTip == "" {
			mainTip = refs[0].oid
		}
		items = append(items, mainTip+" HEAD\x00"+caps+"\n")
		for _, ref := range refs {
			items = append(items, ref.oid+" "+ref.name+"\n")
			if ref.peeled != "" {
				items = append(items, ref.peeled+" "+ref.name+"^{}\n")
			}
		}
	}
	items = append(items, "flush")
	return encodePktLines(items)
}

// v2Advertisement is a v2 capability advertisement for upload-pack (constant).
var v2AdvertisementBytes = encodePktLines([]any{
	"# service=git-upload-pack\n",
	"flush",
	"version 2\n",
	"agent=" + agentString + "\n",
	"ls-refs=unborn\n",
	"fetch=shallow wait-for-done\n",
	"object-format=sha1\n",
	"flush",
})

// handleInfoRefs serves GET /:owner/:repo.git/info/refs?service=...
// Called with the store RLock held; it returns cached bytes when possible.
func handleInfoRefs(s *Store, owner, repoName, service, gitProtocol string) response {
	r := s.getRepo(owner, repoName)
	if r == nil || owner != viewer.login {
		return jsonMsg(404, "Not Found")
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return jsonMsg(400, "unknown service")
	}
	contentType := "application/x-" + service + "-advertisement"
	if service == "git-upload-pack" && gitProtocol == "version=2" {
		return bufResp(200, contentType, v2AdvertisementBytes)
	}
	// The advertisement is rebuilt per request: it is one ref and a few
	// hundred bytes, and a cache shared across concurrent readers would need
	// its own lock for no measurable gain.
	return bufResp(200, contentType, v0Advertisement(r, service))
}

// ---------------------------------------------------------------- ls-refs (v2)

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

// handleUploadPack serves POST /:owner/:repo.git/git-upload-pack — ls-refs or
// fetch, both protocol v2. The pack is built lock-free: objects are selected
// under RLock, then deflated after the lock is released.
func handleUploadPack(s *Store, owner, repoName string, body []byte) response {
	resultType := "application/x-git-upload-pack-result"
	s.mu.RLock()
	r := s.getRepo(owner, repoName)
	if r == nil || owner != viewer.login {
		s.mu.RUnlock()
		return jsonMsg(404, "Not Found")
	}
	packets, err := decodePktLines(body)
	if err != nil {
		// A malformed body must not crash the server: answer an empty result.
		s.mu.RUnlock()
		return bufResp(200, resultType, flushPkt)
	}
	lines := []string{}
	for _, packet := range packets {
		if packet.kind == pktData {
			lines = append(lines, string(packet.payload))
		}
	}
	command := ""
	if len(lines) > 0 {
		command = strings.TrimSpace(lines[0])
	}
	if command == "command=ls-refs" {
		args := lsRefsArgs{}
		for _, line := range lines[1:] {
			text := strings.TrimSpace(line)
			switch {
			case text == "peel":
				args.peel = true
			case text == "symrefs":
				args.symrefs = true
			case text == "unborn":
				args.unborn = true
			case strings.HasPrefix(text, "ref-prefix "):
				args.prefixes = append(args.prefixes, text[len("ref-prefix "):])
			}
		}
		out := lsRefsResponse(r, args)
		s.mu.RUnlock()
		return bufResp(200, resultType, out)
	}
	if command == "command=fetch" {
		// Select objects under the lock, build the pack after releasing it.
		resp := fetchV2Locked(s, r, lines[1:])
		s.mu.RUnlock()
		pack, perr := writePack(resp.selection.objects)
		if perr != nil {
			return jsonMsg(500, "Internal Server Error")
		}
		return bufResp(200, resultType, assembleFetchV2(resp, pack))
	}
	// Not a v2 command: answer an empty result rather than failing the exchange.
	s.mu.RUnlock()
	return bufResp(200, resultType, flushPkt)
}

func lsRefsResponse(r *repo, args lsRefsArgs) []byte {
	refs := sortedRefs(r)
	mainTip, hasMain := r.refs["refs/heads/main"]
	includeHead := len(args.prefixes) == 0 || hasPrefixMatch("HEAD", args.prefixes)
	keep := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		return hasPrefixMatch(name, args.prefixes)
	}
	var out bytes.Buffer
	if len(refs) == 0 {
		// Unborn HEAD: the clone-empty check reads the symref target off this line.
		if includeHead && args.unborn {
			if args.symrefs {
				out.Write(pktLineString("unborn HEAD symref-target:refs/heads/main\n"))
			} else {
				out.Write(pktLineString("unborn HEAD\n"))
			}
		}
	} else {
		if includeHead && hasMain {
			if args.symrefs {
				out.Write(pktLineString(mainTip + " HEAD symref-target:refs/heads/main\n"))
			} else {
				out.Write(pktLineString(mainTip + " HEAD\n"))
			}
		}
		for _, ref := range refs {
			if !keep(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if args.peel && ref.peeled != "" {
				line += " peeled:" + ref.peeled
			}
			out.Write(pktLineString(line + "\n"))
		}
	}
	out.Write(flushPkt)
	return out.Bytes()
}

func hasPrefixMatch(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- fetch (v2)

type fetchPlan struct {
	wants     []string
	haves     []string
	heldHaves []string // haves the store held at selection time
	shallow   []string
	depth     *int
	selection fetchSelection
}

// fetchV2Locked parses fetch args and selects objects (caller holds RLock).
func fetchV2Locked(s *Store, r *repo, argLines []string) fetchPlan {
	plan := fetchPlan{}
	for _, raw := range argLines {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "want "):
			plan.wants = append(plan.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			plan.haves = append(plan.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			plan.shallow = append(plan.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil && n >= 0 {
				if n == 0 {
					plan.depth = nil
				} else {
					d := n
					plan.depth = &d
				}
			}
		}
	}
	for _, have := range plan.haves {
		if _, ok := r.objects[have]; ok {
			plan.heldHaves = append(plan.heldHaves, have)
		}
	}
	plan.selection = selectFetchObjects(r.objects, plan.wants, plan.haves, plan.shallow, plan.depth)
	return plan
}

func assembleFetchV2(plan fetchPlan, pack []byte) []byte {
	var parts bytes.Buffer
	// acknowledgments section.
	parts.Write(pktLineString("acknowledgments\n"))
	if len(plan.haves) == 0 {
		parts.Write(pktLineString("NAK\n"))
	} else {
		for _, have := range plan.heldHaves {
			parts.Write(pktLineString("ACK " + have + "\n"))
		}
		if len(plan.heldHaves) == len(plan.haves) {
			parts.Write(pktLineString("ready\n"))
		} else if len(plan.heldHaves) == 0 {
			parts.Write(pktLineString("NAK\n"))
		}
	}
	parts.Write(delimPkt)
	// shallow-info section, only when there is something to say.
	if len(plan.selection.shallow) > 0 || len(plan.selection.unshallow) > 0 {
		parts.Write(pktLineString("shallow-info\n"))
		for _, oid := range plan.selection.shallow {
			parts.Write(pktLineString("shallow " + oid + "\n"))
		}
		for _, oid := range plan.selection.unshallow {
			parts.Write(pktLineString("unshallow " + oid + "\n"))
		}
		parts.Write(delimPkt)
	}
	// packfile section: always at least one sideband packet.
	parts.Write(pktLineString("packfile\n"))
	const chunk = 32768
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		chunkBuf := make([]byte, 0, 1+end-offset)
		chunkBuf = append(chunkBuf, 1)
		chunkBuf = append(chunkBuf, pack[offset:end]...)
		parts.Write(pktLine(chunkBuf))
	}
	parts.Write(flushPkt)
	return parts.Bytes()
}

// ---------------------------------------------------------------- fetch (v0)

// handleUploadPackV0 serves POST /:owner/:repo.git/git-upload-pack, protocol v0.
func handleUploadPackV0(s *Store, owner, repoName string, body []byte) response {
	resultType := "application/x-git-upload-pack-result"
	s.mu.RLock()
	r := s.getRepo(owner, repoName)
	if r == nil || owner != viewer.login {
		s.mu.RUnlock()
		return jsonMsg(404, "Not Found")
	}
	packets, err := decodePktLines(body)
	if err != nil {
		s.mu.RUnlock()
		return bufResp(200, resultType, flushPkt)
	}
	// Two sections split by the first flush: wants, then haves/done.
	var wants, haves, shallow []string
	var depth *int
	secondSection := false
	for _, packet := range packets {
		if packet.kind == pktFlush {
			secondSection = true
			continue
		}
		if packet.kind != pktData {
			continue
		}
		line := strings.TrimSpace(string(packet.payload))
		if secondSection {
			if strings.HasPrefix(line, "have ") {
				haves = append(haves, strings.TrimSpace(line[5:]))
			}
		} else {
			switch {
			case strings.HasPrefix(line, "want "):
				want := strings.TrimSpace(line[5:])
				if sp := strings.IndexByte(want, ' '); sp >= 0 {
					want = want[:sp]
				}
				wants = append(wants, want)
			case strings.HasPrefix(line, "shallow "):
				shallow = append(shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil && n >= 0 {
					if n == 0 {
						depth = nil
					} else {
						d := n
						depth = &d
					}
				}
			}
		}
	}
	selection := selectFetchObjects(r.objects, wants, haves, shallow, depth)
	var heldHaves []string
	for _, have := range haves {
		if _, ok := r.objects[have]; ok {
			heldHaves = append(heldHaves, have)
		}
	}
	objects := selection.objects
	s.mu.RUnlock()
	pack, perr := writePack(objects)
	if perr != nil {
		return jsonMsg(500, "Internal Server Error")
	}
	var parts bytes.Buffer
	if len(haves) == 0 {
		parts.Write(pktLineString("NAK\n"))
	} else {
		for _, have := range heldHaves {
			parts.Write(pktLineString("ACK " + have + "\n"))
		}
		if len(heldHaves) == 0 {
			parts.Write(pktLineString("NAK\n"))
		}
	}
	for _, oid := range selection.shallow {
		parts.Write(pktLineString("shallow " + oid + "\n"))
	}
	for _, oid := range selection.unshallow {
		parts.Write(pktLineString("unshallow " + oid + "\n"))
	}
	const chunk = 32768
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		chunkBuf := make([]byte, 0, 1+end-offset)
		chunkBuf = append(chunkBuf, 1)
		chunkBuf = append(chunkBuf, pack[offset:end]...)
		parts.Write(pktLine(chunkBuf))
	}
	parts.Write(flushPkt)
	return bufResp(200, resultType, parts.Bytes())
}

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

// handleReceivePack serves POST /:owner/:repo.git/git-receive-pack.
func handleReceivePack(s *Store, owner, repoName string, body []byte) response {
	resultType := "application/x-git-receive-pack-result"
	report := func(lines []string, sideband bool) response {
		// Each report line is pkt-framed inside band 1 (the client's
		// parsePushReport decodes the concatenated band-1 bytes as pkt-lines);
		// without sideband, plain pkt-lines.
		var parts bytes.Buffer
		for _, line := range lines {
			inner := pktLineString(line)
			if sideband {
				band := make([]byte, 0, len(inner)+1)
				band = append(band, 1)
				band = append(band, inner...)
				parts.Write(pktLine(band))
			} else {
				parts.Write(inner)
			}
		}
		parts.Write(flushPkt)
		return bufResp(200, resultType, parts.Bytes())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.getRepo(owner, repoName)
	if r == nil || owner != viewer.login {
		return jsonMsg(404, "Not Found")
	}

	var commands []pushCommand
	sideband := false
	packOffset := 0
	packets, offset, err := decodePktLinesUntilFlush(body)
	if err != nil {
		// Unparseable command list: answer unpack ok with no ref lines.
		return report([]string{"unpack ok\n"}, sideband)
	}
	packOffset = offset
	first := true
	badCommand := false
	for _, packet := range packets {
		if packet.kind == pktFlush {
			break
		}
		if packet.kind != pktData {
			continue
		}
		text := string(packet.payload)
		line := strings.TrimSuffix(text, "\n")
		nul := strings.IndexByte(line, 0)
		head := line
		if nul >= 0 {
			head = line[:nul]
		}
		if first && nul >= 0 {
			caps := strings.Split(line[nul+1:], " ")
			for _, cap := range caps {
				if cap == "side-band-64k" {
					sideband = true
				}
			}
		}
		first = false
		parts := strings.SplitN(head, " ", 3)
		if len(parts) != 3 || len(parts[0]) != 40 || len(parts[1]) != 40 || parts[2] == "" ||
			strings.ContainsAny(parts[2], " \t") || !isHex40(parts[0]) || !isHex40(parts[1]) {
			badCommand = true
			break
		}
		commands = append(commands, pushCommand{oldOid: parts[0], newOid: parts[1], ref: parts[2]})
	}
	if badCommand {
		return report([]string{"unpack ok\n"}, sideband)
	}

	unpackOk := true
	unpackDetail := "ok"
	packBytes := body[packOffset:]
	if len(packBytes) > 0 {
		objects, err := readPack(packBytes, nil)
		if err != nil {
			unpackOk = false
			unpackDetail = strings.ReplaceAll(err.Error(), "\n", " ")
		} else {
			for oid, obj := range objects {
				if _, ok := r.objects[oid]; !ok {
					r.objects[oid] = obj
				}
			}
		}
	}

	lines := []string{"unpack " + map[bool]string{true: "ok", false: unpackDetail}[unpackOk] + "\n"}
	for _, command := range commands {
		if !unpackOk {
			lines = append(lines, "ng "+command.ref+" unpack failed\n")
			continue
		}
		current, exists := r.refs[command.ref]
		switch {
		case command.newOid == zeroOID:
			// Delete.
			if !exists {
				lines = append(lines, "ng "+command.ref+" no such ref\n")
			} else if current != command.oldOid {
				lines = append(lines, "ng "+command.ref+" non-fast-forward\n")
			} else {
				delete(r.refs, command.ref)
				lines = append(lines, "ok "+command.ref+"\n")
			}
		case command.oldOid == zeroOID:
			// Create.
			if exists {
				lines = append(lines, "ng "+command.ref+" already exists\n")
			} else if _, ok := r.objects[command.newOid]; !ok {
				lines = append(lines, "ng "+command.ref+" object not found\n")
			} else {
				r.refs[command.ref] = command.newOid
				lines = append(lines, "ok "+command.ref+"\n")
			}
		default:
			// Update: old must match the tip and new must descend from old.
			if !exists {
				lines = append(lines, "ng "+command.ref+" no such ref\n")
			} else if current != command.oldOid {
				lines = append(lines, "ng "+command.ref+" non-fast-forward\n")
			} else if _, ok := r.objects[command.newOid]; !ok {
				lines = append(lines, "ng "+command.ref+" object not found\n")
			} else if !s.isAncestor(r, command.oldOid, command.newOid) {
				lines = append(lines, "ng "+command.ref+" non-fast-forward\n")
			} else {
				r.refs[command.ref] = command.newOid
				lines = append(lines, "ok "+command.ref+"\n")
			}
		}
	}
	return report(lines, sideband)
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

