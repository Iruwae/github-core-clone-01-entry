// The server: one in-memory forge per process. Routing, auth and the
// request-id header live here; the git, REST, GraphQL and MCP surfaces live in
// their own files.
//
// Auth: every route except GET /healthz requires `Authorization: Bearer
// <token>`. When MAKEFUN_SHOP_TOKEN is set (a trial), the token must equal it;
// when it is unset (local practice), any non-empty bearer is accepted and a
// missing one is a 401. Every reply carries a non-empty X-GitHub-Request-Id
// (an incrementing counter — no randomness, so a live trial and its replay
// answer identically).

package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

const maxBodyBytes = 64 * 1024 * 1024

// request is one parsed HTTP request, as the handlers see it.
type request struct {
	method      string
	path        string
	host        string
	query       url.Values
	body        []byte
	auth        string
	gitProtocol string
	// rawStatus is set by the raw transport when the request itself is
	// unusable (413: body over the cap). serveRequest answers it directly.
	rawStatus int
	// segments is the decoded path split, computed once in route() and
	// reused by the REST router (it used to re-split the same path).
	segments []string
}

// response is what a handler returns; the server writes it out.
type response struct {
	status      int
	contentType string
	body        []byte
	headers     map[string]string
}

// jsonBytes builds a JSON response: the body plus one trailing newline, as
// the reference's sendJson does.
func jsonBytes(status int, body []byte) response {
	out := make([]byte, 0, len(body)+1)
	out = append(out, body...)
	out = append(out, '\n')
	return response{status: status, contentType: "application/json; charset=utf-8", body: out}
}

// bufResp builds a raw (non-JSON) response with the given content type.
func bufResp(status int, contentType string, body []byte) response {
	return response{status: status, contentType: contentType, body: body}
}

// emptyResp builds a response with no body.
func emptyResp(status int) response {
	return response{status: status}
}

var requestCounter atomic.Int64

var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

func authorized(authHeader string) bool {
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}
	token := authHeader[len("Bearer "):]
	if token == "" {
		return false
	}
	if shopToken != "" {
		return token == shopToken
	}
	return true
}

// splitTarget splits a request target into its %-encoded path and query.
// No query string (the timed read/write/push/clone requests) returns a nil
// query: ParseQuery on "" still allocates a map, and every reader here is
// nil-safe (Get on a nil map returns "", ranging a nil map is empty).
func splitTarget(target string) (string, url.Values) {
	rawPath := target
	rawQuery := ""
	if idx := strings.Index(target, "?"); idx >= 0 {
		rawPath = target[:idx]
		rawQuery = target[idx+1:]
	}
	if rawPath == "" {
		rawPath = "/"
	}
	if rawQuery == "" {
		return rawPath, nil
	}
	query, _ := url.ParseQuery(rawQuery)
	return rawPath, query
}

// route dispatches one request to the right surface.
func route(s *Store, req *request) response {
	path := req.path

	// MCP: streamable HTTP on POST /mcp (DELETE closes a session; answer 200).
	if path == "/mcp" {
		if req.method == "POST" {
			return handleMcp(s, req)
		}
		if req.method == "DELETE" {
			return emptyResp(200)
		}
		return jsonMsg(404, "Not Found")
	}

	// GraphQL v4.
	if path == "/api/graphql" || path == "/api/graphql/" {
		if req.method == "POST" {
			return handleGraphQL(s, req)
		}
		return jsonMsg(404, "Not Found")
	}

	// Git smart HTTP: /<owner>/<repo>.git/<service-path>.
	segments := []string{}
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			segments = append(segments, decodeSegment(part))
		}
	}
	req.segments = segments
	if len(segments) >= 3 && strings.HasSuffix(segments[1], ".git") &&
		(segments[2] == "info" || segments[2] == "git-upload-pack" || segments[2] == "git-receive-pack") {
		owner := segments[0]
		repoName := strings.TrimSuffix(segments[1], ".git")
		if req.method == "GET" && segments[2] == "info" && len(segments) >= 4 && segments[3] == "refs" {
			return handleInfoRefs(s, owner, repoName, req.query.Get("service"), req.gitProtocol)
		}
		if req.method == "POST" && segments[2] == "git-upload-pack" {
			if req.gitProtocol == "version=2" {
				return handleUploadPack(s, owner, repoName, req.body)
			}
			return handleUploadPackV0(s, owner, repoName, req.body)
		}
		if req.method == "POST" && segments[2] == "git-receive-pack" {
			return handleReceivePack(s, owner, repoName, req.body)
		}
		return jsonMsg(404, "Not Found")
	}

	// REST v3: /api/v3 and everything under it.
	if path == "/api/v3" || path == "/api/v3/" || strings.HasPrefix(path, "/api/v3/") {
		return handleRest(s, req)
	}

	// Everything else, every method: GitHub's 404 body.
	return jsonMsg(404, "Not Found")
}

// withRequestID stamps a response with a fresh X-GitHub-Request-Id, as
// every reply carries one.
func withRequestID(resp response) response {
	id := requestCounter.Add(1)
	value := "req-" + strconv.FormatInt(id, 10)
	if resp.headers == nil {
		resp.headers = map[string]string{"x-github-request-id": value}
	} else {
		resp.headers["x-github-request-id"] = value
	}
	return resp
}

// serveRequest is the transport-independent core: the response for one parsed
// request. Both the net/http handler and the raw HTTP/1.1 transport call it,
// so their behavior is identical. It recovers from handler panics (500) and
// stamps the request id on every reply.
func serveRequest(s *Store, req *request) (resp response) {
	// The deferred stamp runs on every exit — including a recovered panic —
	// so the 500 also carries the request id, as before.
	defer func() {
		if rec := recover(); rec != nil {
			resp = jsonMsg(500, "Internal Server Error")
		}
		resp = withRequestID(resp)
	}()

	// The env's boot check calls this twice: 200 {"ok":true}. No auth.
	if req.rawStatus == 413 {
		resp = jsonMsg(413, "request body too large")
	} else if req.method == "GET" && req.path == "/healthz" {
		resp = jsonBytes(200, []byte(`{"ok":true}`))
	} else if !authorized(req.auth) {
		resp = jsonMsg(401, "Bad credentials")
	} else {
		resp = route(s, req)
	}
	return resp
}

// handler is the http.Handler for the whole forge (kept as the fallback
// transport; main uses the raw HTTP/1.1 server).
type handler struct {
	store *Store
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawPath, query := splitTarget(r.RequestURI)
	method := r.Method

	// Read the request body fully (up to the cap); empty for GET/HEAD.
	var body []byte
	if method != "GET" && method != "HEAD" {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			writeResponse(w, withRequestID(jsonMsg(500, "Internal Server Error")))
			return
		}
		if len(data) > maxBodyBytes {
			writeResponse(w, withRequestID(jsonMsg(413, "request body too large")))
			return
		}
		body = data
	}

	req := &request{
		method:      method,
		path:        rawPath,
		host:        r.Host,
		query:       query,
		body:        body,
		auth:        r.Header.Get("Authorization"),
		gitProtocol: r.Header.Get("Git-Protocol"),
	}
	writeResponse(w, serveRequest(h.store, req))
}

func writeResponse(w http.ResponseWriter, resp response) {
	if resp.contentType != "" {
		w.Header().Set("Content-Type", resp.contentType)
	}
	for k, v := range resp.headers {
		w.Header().Set(k, v)
	}
	if resp.body != nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(resp.body)))
	}
	w.WriteHeader(resp.status)
	if len(resp.body) > 0 {
		_, _ = w.Write(resp.body)
	}
}
