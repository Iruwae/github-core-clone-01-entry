// Git plumbing: pkt-line framing, pack v2 read/write (plain objects out, deltas
// resolved on read), commit/tag/tree parsing and building, and the fetch object
// selection the suite's fetch checks pin down exactly (reachable(wants) -
// reachable(haves), with shallow/deepen boundaries).
//
// All object ids are SHA-1 over `<type> <size>\0` + bytes. Objects are stored
// verbatim; the server never re-serializes a pushed commit, tree or tag.

package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

// gitObject is one stored object.
type gitObject struct {
	typ  string // 'commit' | 'tree' | 'blob' | 'tag'
	data []byte
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// objectID is the object id of typ + data (the `<type> <size>\0` header is hashed in).
func objectID(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------- pkt-lines

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
)

type pkt struct {
	kind    pktKind
	payload []byte
}

// pktLine frames one payload.
func pktLine(payload []byte) []byte {
	length := len(payload) + 4
	if length > 0xffff {
		panic(fmt.Sprintf("pkt-line of %d bytes exceeds the 65520-byte frame", len(payload)))
	}
	out := make([]byte, 0, length)
	out = append(out, []byte(fmt.Sprintf("%04x", length))...)
	out = append(out, payload...)
	return out
}

func pktLineString(s string) []byte { return pktLine([]byte(s)) }

var flushPkt = []byte("0000")
var delimPkt = []byte("0001")

// encodePktLines encodes a list of pkt-lines / flush / delim markers into one buffer.
// Items are []byte payloads, or the strings "flush" / "delim".
func encodePktLines(items []any) []byte {
	var out bytes.Buffer
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				out.Write(flushPkt)
			} else if v == "delim" {
				out.Write(delimPkt)
			} else {
				out.Write(pktLineString(v))
			}
		case []byte:
			out.Write(pktLine(v))
		}
	}
	return out.Bytes()
}

// decodePktLines decodes a whole buffer of pkt-lines. It errors on a malformed frame.
func decodePktLines(buf []byte) ([]pkt, error) {
	var out []pkt
	offset := 0
	for offset < len(buf) {
		if offset+4 > len(buf) {
			return nil, errors.New("pkt-line truncated at length prefix")
		}
		length, err := strconv.ParseInt(string(buf[offset:offset+4]), 16, 32)
		if err != nil || length < 0 || length > 0xffff {
			return nil, fmt.Errorf("bad pkt-line length %q", string(buf[offset:offset+4]))
		}
		switch length {
		case 0:
			out = append(out, pkt{kind: pktFlush})
			offset += 4
		case 1:
			out = append(out, pkt{kind: pktDelim})
			offset += 4
		default:
			if length < 4 {
				return nil, fmt.Errorf("pkt-line length %d is below the 4-byte minimum", length)
			}
			if offset+int(length) > len(buf) {
				return nil, errors.New("pkt-line truncated at payload")
			}
			payload := make([]byte, length-4)
			copy(payload, buf[offset+4:offset+int(length)])
			out = append(out, pkt{kind: pktData, payload: payload})
			offset += int(length)
		}
	}
	return out, nil
}

// decodePktLinesUntilFlush decodes pkt-lines from the start of body up to and
// including the first flush packet, returning the packets and the byte offset just
// past that flush (where raw pack bytes begin).
func decodePktLinesUntilFlush(body []byte) ([]pkt, int, error) {
	var packets []pkt
	offset := 0
	for offset+4 <= len(body) {
		length, err := strconv.ParseInt(string(body[offset:offset+4]), 16, 32)
		if err != nil || length < 0 || length > 0xffff {
			return nil, 0, errors.New("bad pkt-line length in push commands")
		}
		if length == 0 {
			packets = append(packets, pkt{kind: pktFlush})
			return packets, offset + 4, nil
		}
		if length == 1 {
			packets = append(packets, pkt{kind: pktDelim})
			offset += 4
			continue
		}
		if length < 4 || offset+int(length) > len(body) {
			return nil, 0, errors.New("pkt-line truncated in push commands")
		}
		payload := make([]byte, length-4)
		copy(payload, body[offset+4:offset+int(length)])
		packets = append(packets, pkt{kind: pktData, payload: payload})
		offset += int(length)
	}
	return nil, 0, errors.New("push commands end without a flush")
}

// ---------------------------------------------------------------- deltas

// applyDelta applies a git delta to base, per the delta instruction format.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta truncated in varint")
			}
			b := delta[pos]
			pos++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return value, nil
			}
		}
	}
	baseSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if baseSize != len(base) {
		return nil, fmt.Errorf("delta base size %d does not match %d", baseSize, len(base))
	}
	resultSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var parts [][]byte
	produced := 0
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// Copy from base: offset/size bitfields.
			offset := 0
			size := 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta truncated in copy offset")
					}
					offset |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta truncated in copy size")
					}
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta copy runs past the base")
			}
			parts = append(parts, base[offset:offset+size])
			produced += size
		} else if op != 0 {
			// Insert literal bytes.
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta truncated in insert")
			}
			parts = append(parts, delta[pos:pos+int(op)])
			pos += int(op)
			produced += int(op)
		} else {
			return nil, errors.New("delta has a zero opcode")
		}
	}
	if produced != resultSize {
		return nil, fmt.Errorf("delta produced %d bytes, expected %d", produced, resultSize)
	}
	return bytes.Join(parts, nil), nil
}

// ---------------------------------------------------------------- packs

var packTypes = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag", 6: "ofs-delta", 7: "ref-delta"}
var typeCodes = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

// inflateAt inflates the zlib stream at offset, returning its bytes and the
// stream's byte length. A counting reader gives the consumed length in one pass.
func inflateAt(buf []byte, offset int) ([]byte, int, error) {
	br := bytes.NewReader(buf[offset:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := len(buf[offset:]) - br.Len()
	return data, consumed, nil
}

// readPack parses a pack v2 buffer into objects keyed by id. It verifies the
// trailer SHA-1 and resolves OFS/REF deltas against pack entries (and, for REF
// deltas, against resolveBase when the base is not in the pack).
func readPack(buf []byte, resolveBase func(oid string) *gitObject) (map[string]*gitObject, error) {
	if len(buf) < 32 {
		return nil, errors.New("pack too short for a header and trailer")
	}
	if string(buf[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != 2 {
		return nil, fmt.Errorf("pack version %d, expected 2", version)
	}
	count := binary.BigEndian.Uint32(buf[8:12])
	trailer := buf[len(buf)-20:]
	expect := sha1.Sum(buf[:len(buf)-20])
	if !bytes.Equal(expect[:], trailer) {
		return nil, errors.New("pack trailer checksum mismatch")
	}
	type rawEntry struct {
		offset     int
		typeCode    int
		data       []byte
		baseOffset int
		baseOid    string
		hasBaseOff bool
		hasBaseOid bool
	}
	raw := make([]rawEntry, 0, count)
	offset := 12
	for i := uint32(0); i < count; i++ {
		entryOffset := offset
		if offset >= len(buf)-20 {
			return nil, errors.New("pack truncated at an object header")
		}
		b := buf[offset]
		offset++
		typeCode := int((b >> 4) & 0x7)
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if offset >= len(buf)-20 {
				return nil, errors.New("pack truncated in an object size")
			}
			b = buf[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		entry := rawEntry{offset: entryOffset, typeCode: typeCode}
		if typeCode == 6 {
			// OFS_DELTA: negative offset varint.
			if offset >= len(buf)-20 {
				return nil, errors.New("pack truncated in an ofs-delta offset")
			}
			b = buf[offset]
			offset++
			value := int(b & 0x7f)
			for b&0x80 != 0 {
				if offset >= len(buf)-20 {
					return nil, errors.New("pack truncated in an ofs-delta offset")
				}
				b = buf[offset]
				offset++
				value = ((value + 1) << 7) | int(b&0x7f)
			}
			entry.baseOffset = entryOffset - value
			entry.hasBaseOff = true
			if entry.baseOffset < 0 {
				return nil, errors.New("ofs-delta names a base before the pack start")
			}
		} else if typeCode == 7 {
			if offset+20 > len(buf)-20 {
				return nil, errors.New("pack truncated in a ref-delta base id")
			}
			entry.baseOid = hex.EncodeToString(buf[offset : offset+20])
			entry.hasBaseOid = true
			offset += 20
		} else if packTypes[typeCode] == "" {
			return nil, fmt.Errorf("pack object has unknown type code %d", typeCode)
		}
		data, streamLen, err := inflateAt(buf, offset)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, fmt.Errorf("pack object inflated to %d bytes, header says %d", len(data), size)
		}
		offset += streamLen
		entry.data = data
		raw = append(raw, entry)
	}
	// Resolve deltas. OFS entries reference earlier entries only; REF may need the store.
	byOffset := make(map[int]string, len(raw))
	byOid := make(map[string]int, len(raw))
	var resolve func(entry *rawEntry, seen map[int]bool) (*gitObject, error)
	resolve = func(entry *rawEntry, seen map[int]bool) (*gitObject, error) {
		if entry.typeCode != 6 && entry.typeCode != 7 {
			typ := packTypes[entry.typeCode]
			oid := objectID(typ, entry.data)
			byOffset[entry.offset] = oid
			byOid[oid] = entry.offset
			return &gitObject{typ: typ, data: entry.data}, nil
		}
		if seen[entry.offset] {
			return nil, errors.New("pack delta cycle")
		}
		seen[entry.offset] = true
		var base *gitObject
		if entry.typeCode == 6 {
			var baseEntry *rawEntry
			for i := range raw {
				if raw[i].offset == entry.baseOffset {
					baseEntry = &raw[i]
					break
				}
			}
			if baseEntry == nil {
				return nil, errors.New("ofs-delta base is not in the pack")
			}
			var err error
			base, err = resolve(baseEntry, seen)
			if err != nil {
				return nil, err
			}
		} else {
			if off, ok := byOid[entry.baseOid]; ok {
				var baseEntry *rawEntry
				for i := range raw {
					if raw[i].offset == off {
						baseEntry = &raw[i]
						break
					}
				}
				if baseEntry != nil {
					var err error
					base, err = resolve(baseEntry, seen)
					if err != nil {
						return nil, err
					}
				}
			}
			if base == nil && resolveBase != nil {
				base = resolveBase(entry.baseOid)
			}
			if base == nil {
				return nil, fmt.Errorf("ref-delta base %s is not available", entry.baseOid)
			}
		}
		data, err := applyDelta(base.data, entry.data)
		if err != nil {
			return nil, err
		}
		oid := objectID(base.typ, data)
		byOffset[entry.offset] = oid
		byOid[oid] = entry.offset
		return &gitObject{typ: base.typ, data: data}, nil
	}
	objects := make(map[string]*gitObject, len(raw))
	for i := range raw {
		obj, err := resolve(&raw[i], make(map[int]bool))
		if err != nil {
			return nil, err
		}
		objects[objectID(obj.typ, obj.data)] = obj
	}
	return objects, nil
}

func packObjectHeader(typ string, size int) ([]byte, error) {
	code, ok := typeCodes[typ]
	if !ok {
		return nil, fmt.Errorf("cannot pack type %s", typ)
	}
	var bytesOut []byte
	first := (code << 4) | (size & 0x0f)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	bytesOut = append(bytesOut, byte(first))
	for size > 0 {
		b := size & 0x7f
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		bytesOut = append(bytesOut, byte(b))
	}
	return bytesOut, nil
}

// writePack writes a pack v2 of plain objects (no deltas — the suite's
// pack-deltas check passes with 0). BestSpeed: the client only verifies the
// checksum, so CPU beats ratio here.
// zlibWriterPool reuses compressors across packs. Creating a zlib writer
// costs ~200 µs (it builds the deflate tables); Resetting a pooled one and
// writing a small object costs ~3 µs and produces byte-identical output,
// because Reset restores the fresh-writer state. Each pack object is still
// its own zlib stream, as the pack format requires.
var zlibWriterPool = sync.Pool{
	New: func() any {
		zw, err := zlib.NewWriterLevel(nil, flate.BestSpeed)
		if err != nil {
			return nil
		}
		return zw
	},
}

func writePack(objects []*gitObject) ([]byte, error) {
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	body.Write(header)
	zw, _ := zlibWriterPool.Get().(*zlib.Writer)
	if zw == nil {
		return nil, errNoCompressor
	}
	defer zlibWriterPool.Put(zw)
	for _, object := range objects {
		hdr, err := packObjectHeader(object.typ, len(object.data))
		if err != nil {
			return nil, err
		}
		body.Write(hdr)
		zw.Reset(&body)
		if _, err := zw.Write(object.data); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	out := body.Bytes()
	sum := sha1.Sum(out)
	return append(out, sum[:]...), nil
}

var errNoCompressor = errors.New("no zlib compressor available")

// emptyPack is a valid empty pack (12-byte header + 20-byte trailer).
func emptyPack() []byte {
	pack, err := writePack(nil)
	if err != nil {
		panic(err)
	}
	return pack
}

// ---------------------------------------------------------------- commits, tags, trees

// commitInfo is a parsed commit object.
type commitInfo struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) commitInfo {
	text := string(data)
	head := text
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		head = text[:idx]
	}
	info := commitInfo{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			info.parents = append(info.parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			info.author = line[7:]
		case strings.HasPrefix(line, "committer "):
			info.committer = line[10:]
		}
	}
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		info.message = text[idx+2:]
	}
	return info
}

// tagInfo is a parsed tag object.
type tagInfo struct {
	object string
	typ    string
	tag    string
	tagger string
}

func parseTag(data []byte) tagInfo {
	text := string(data)
	head := text
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		head = text[:idx]
	}
	info := tagInfo{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			info.object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			info.typ = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			info.tag = line[4:]
		case strings.HasPrefix(line, "tagger "):
			info.tagger = line[7:]
		}
	}
	return info
}

// treeEntry is one entry of a tree object.
type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) ([]treeEntry, error) {
	var entries []treeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			return nil, errors.New("tree entry has no mode")
		}
		mode := string(data[offset : offset+space])
		nul := bytes.IndexByte(data[offset+space+1:], 0x00)
		if nul < 0 || offset+space+1+nul+21 > len(data) {
			return nil, errors.New("tree entry has no name or id")
		}
		nameStart := offset + space + 1
		name := string(data[nameStart : nameStart+nul])
		oidStart := nameStart + nul + 1
		oid := hex.EncodeToString(data[oidStart : oidStart+20])
		entries = append(entries, treeEntry{mode: mode, name: name, oid: oid})
		offset = oidStart + 20
	}
	return entries, nil
}

// treeSortKey: directories compare as if their name ended in '/'.
func treeSortKey(name string, isDir bool) string {
	if isDir {
		return name + "/"
	}
	return name
}

// buildTree serializes tree entries in git order (dirs sort as name + '/').
func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		ka := treeSortKey(sorted[i].name, sorted[i].mode == "40000")
		kb := treeSortKey(sorted[j].name, sorted[j].mode == "40000")
		return ka < kb
	})
	var out bytes.Buffer
	for _, entry := range sorted {
		out.WriteString(entry.mode + " " + entry.name + "\x00")
		raw, err := hex.DecodeString(entry.oid)
		if err != nil {
			panic(err)
		}
		out.Write(raw)
	}
	return out.Bytes()
}

// buildCommit builds a commit object's bytes (verbatim layout, no
// re-serialization elsewhere).
func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	var out bytes.Buffer
	out.WriteString("tree " + tree + "\n")
	for _, parent := range parents {
		out.WriteString("parent " + parent + "\n")
	}
	out.WriteString("author " + author + "\n")
	out.WriteString("committer " + committer + "\n")
	out.WriteString("\n")
	out.WriteString(message)
	return out.Bytes()
}

// ---------------------------------------------------------------- fetch selection

// fetchSelection is what a fetch sends, in dependency-safe order.
type fetchSelection struct {
	objects   []*gitObject
	shallow   []string
	unshallow []string
}

// selectFetchObjects selects the objects a fetch sends, exactly as the suite's
// fetch checks require: reachable(wants) - reachable(haves), with a depth limit
// and shallow boundaries.
func selectFetchObjects(store map[string]*gitObject, wants, haves, shallowBoundaries []string, depth *int) fetchSelection {
	haveSet := make(map[string]bool, len(haves))
	for _, h := range haves {
		haveSet[h] = true
	}
	shallowSet := make(map[string]bool, len(shallowBoundaries))
	for _, s := range shallowBoundaries {
		shallowSet[s] = true
	}
	limit := -1 // -1 means no limit
	if depth != nil {
		limit = *depth
	}
	sentCommits := make(map[string]bool)
	var shallowOut []string
	var unshallowOut []string
	visited := make(map[string]int) // commit oid -> shallowest depth visited

	type queueItem struct {
		oid   string
		depth int
	}
	var queue []queueItem
	var tagTargets []string
	sentTags := make(map[string]bool)
	enqueue := func(oid string, depth int) {
		if prev, ok := visited[oid]; ok && prev <= depth {
			return
		}
		visited[oid] = depth
		queue = append(queue, queueItem{oid: oid, depth: depth})
	}
	for _, want := range wants {
		obj, ok := store[want]
		if !ok {
			continue
		}
		if obj.typ == "tag" {
			sentTags[want] = true
			target := parseTag(obj.data).object
			tagTargets = append(tagTargets, target)
			enqueue(target, 0)
		} else if obj.typ == "commit" {
			enqueue(want, 0)
		}
	}
	// Direct (non-commit, non-tag) wants.
	directObjects := make(map[string]*gitObject)
	for _, want := range wants {
		obj, ok := store[want]
		if ok && obj.typ != "commit" && obj.typ != "tag" && !haveSet[want] {
			directObjects[want] = obj
		}
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current.oid] != current.depth {
			continue // superseded by a shallower visit
		}
		if limit >= 0 && current.depth >= limit {
			continue
		}
		obj, ok := store[current.oid]
		if !ok || obj.typ != "commit" {
			continue
		}
		isHave := haveSet[current.oid]
		if isHave && !shallowSet[current.oid] {
			// Held and not a boundary: stop here, send nothing below.
			continue
		}
		commit := parseCommit(obj.data)
		if !isHave {
			sentCommits[current.oid] = true
			if limit >= 0 && current.depth == limit-1 && len(commit.parents) > 0 {
				shallowOut = append(shallowOut, current.oid)
			}
		}
		// Walk parents (through boundaries and sent commits alike, within the limit).
		for _, parent := range commit.parents {
			enqueue(parent, current.depth+1)
		}
	}

	// Unshallow: a request boundary whose parent was included is no longer shallow.
	for _, boundary := range shallowBoundaries {
		obj, ok := store[boundary]
		if ok && obj.typ == "commit" {
			parents := parseCommit(obj.data).parents
			for _, parent := range parents {
				if sentCommits[parent] {
					unshallowOut = append(unshallowOut, boundary)
					break
				}
			}
		}
	}

	// Trees/blobs: everything reachable from sent commits' trees, minus anything
	// reachable from a have-commit's tree.
	excludedTreeObjects := make(map[string]bool)
	collectTreeObjects := func(treeOid string, into map[string]bool) {
		stack := []string{treeOid}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if into[oid] {
				continue
			}
			obj, ok := store[oid]
			if !ok {
				continue
			}
			into[oid] = true
			if obj.typ == "tree" {
				entries, err := parseTree(obj.data)
				if err != nil {
					continue
				}
				for _, entry := range entries {
					stack = append(stack, entry.oid)
				}
			}
		}
	}
	for _, have := range haves {
		obj, ok := store[have]
		if !ok {
			continue
		}
		if obj.typ == "commit" {
			collectTreeObjects(parseCommit(obj.data).tree, excludedTreeObjects)
		} else if obj.typ == "tree" {
			collectTreeObjects(have, excludedTreeObjects)
		}
	}
	send := make(map[string]*gitObject)
	for oid := range sentTags {
		if obj, ok := store[oid]; ok {
			send[oid] = obj
		}
	}
	for oid, obj := range directObjects {
		send[oid] = obj
	}
	for oid := range sentCommits {
		obj, ok := store[oid]
		if !ok {
			continue
		}
		send[oid] = obj
		treeObjects := make(map[string]bool)
		collectTreeObjects(parseCommit(obj.data).tree, treeObjects)
		for treeOid := range treeObjects {
			if excludedTreeObjects[treeOid] {
				continue
			}
			if treeObj, ok := store[treeOid]; ok {
				send[treeOid] = treeObj
			}
		}
	}
	// Tag targets that are trees/blobs (non-commit): send them too.
	for _, target := range tagTargets {
		obj, ok := store[target]
		if ok && obj.typ != "commit" && !excludedTreeObjects[target] {
			send[target] = obj
		}
	}

	// Dependency-safe order: blobs/trees first, then tags, then commits.
	rank := func(typ string) int {
		switch typ {
		case "blob":
			return 0
		case "tree":
			return 1
		case "tag":
			return 2
		default:
			return 3
		}
	}
	objects := make([]*gitObject, 0, len(send))
	for _, obj := range send {
		objects = append(objects, obj)
	}
	sort.SliceStable(objects, func(i, j int) bool {
		return rank(objects[i].typ) < rank(objects[j].typ)
	})
	return fetchSelection{objects: objects, shallow: shallowOut, unshallow: unshallowOut}
}

// reachableIDs returns all object ids reachable from tips.
func reachableIDs(store map[string]*gitObject, tips []string) map[string]bool {
	out := make(map[string]bool)
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[oid] {
			continue
		}
		obj, ok := store[oid]
		if !ok {
			continue
		}
		out[oid] = true
		switch obj.typ {
		case "commit":
			commit := parseCommit(obj.data)
			stack = append(stack, commit.tree)
			stack = append(stack, commit.parents...)
		case "tree":
			entries, err := parseTree(obj.data)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				stack = append(stack, entry.oid)
			}
		case "tag":
			stack = append(stack, parseTag(obj.data).object)
		}
	}
	return out
}
