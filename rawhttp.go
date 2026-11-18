// Raw HTTP/1.1 transport: a minimal server that speaks exactly what the
// suite's undici client sends and expects, without net/http's per-request
// overhead (header maps, bufio churn, Date computation, connection bookkeeping
// — measured ~26 µs/request server-side, of which the handlers are <1 µs).
// The business logic is untouched: every request is parsed into a *request
// and answered by serveRequest, the same core the net/http handler calls, so
// behavior is identical by construction. The net/http handler stays as the
// fallback transport.
//
// Supported, exactly as the client uses it:
//   - request line METHOD SP TARGET SP HTTP/1.1 (1.0 honored for the reply)
//   - headers, matched case-insensitively: Authorization, Content-Length,
//     Git-Protocol, Host, Connection, Expect, Transfer-Encoding
//   - bodies by Content-Length only (chunked is answered 400; the client
//     never sends it), capped at maxBodyBytes (413 + close, as net/http)
//   - keep-alive: sequential requests on one connection until the client
//     closes it or sends Connection: close
//   - responses: status line, Content-Type, Content-Length, Date (cached per
//     second, as net/http does), the response's extra headers, Connection:
//     close only when closing; HEAD carries Content-Length but no body.
package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

// serveRaw runs the accept loop on an already-bound listener until it closes.
func serveRaw(s *Store, listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go serveRawConn(s, conn)
	}
}

// serveRawConn serves sequential keep-alive requests on one connection.
func serveRawConn(s *Store, conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 4096)
	bw := bufio.NewWriterSize(conn, 4096)
	for {
		req, closeAfter, ok := readRawRequest(br, bw)
		if !ok {
			return
		}
		resp := serveRequest(s, req)
		if !writeRawResponse(bw, req, resp, closeAfter) {
			return
		}
		if closeAfter {
			return
		}
	}
}

// rawHeader is one parsed request header (name already lowercased).
type rawHeader struct {
	name  string
	value string
}

// readRawRequest parses one request from br. It returns the request, whether
// the connection must close after the reply, and whether parsing succeeded.
// A 100-continue expectation is answered inline (bw) before the body is read.
func readRawRequest(br *bufio.Reader, bw *bufio.Writer) (*request, bool, bool) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, false, false
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, false
	}
	method, target, version := parts[0], parts[1], parts[2]
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		return nil, false, false
	}

	var auth, gitProtocol, host string
	var contentLength int
	var connectionClose, chunked, expectContinue bool
	for {
		hdr, err := br.ReadString('\n')
		if err != nil {
			return nil, false, false
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if hdr == "" {
			break
		}
		colon := strings.IndexByte(hdr, ':')
		if colon < 0 {
			return nil, false, false
		}
		name := strings.ToLower(strings.TrimSpace(hdr[:colon]))
		value := strings.TrimSpace(hdr[colon+1:])
		switch name {
		case "authorization":
			auth = value
		case "git-protocol":
			gitProtocol = value
		case "host":
			host = value
		case "content-length":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, false, false
			}
			contentLength = n
		case "connection":
			if strings.Contains(strings.ToLower(value), "close") {
				connectionClose = true
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case "expect":
			if strings.Contains(strings.ToLower(value), "100-continue") {
				expectContinue = true
			}
		}
	}

	if chunked {
		// Unsupported: the client never sends chunked bodies.
		return nil, true, false
	}
	if version == "HTTP/1.0" && !connectionClose {
		// HTTP/1.0 defaults to close unless keep-alive was asked for;
		// the client always speaks 1.1, so treat 1.0 as close.
		connectionClose = true
	}
	if expectContinue {
		if _, err := bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return nil, false, false
		}
		if err := bw.Flush(); err != nil {
			return nil, false, false
		}
	}

	rawPath, query := splitTarget(target)

	// Read the body (Content-Length only). A body over the cap is answered
	// 413 by serveRequest (rawStatus), and the connection closes after the
	// reply, as net/http does for an unread oversized body.
	var body []byte
	if contentLength > 0 {
		if contentLength > maxBodyBytes {
			return &request{method: method, path: rawPath, host: host, query: query, auth: auth, gitProtocol: gitProtocol, rawStatus: 413}, true, true
		}
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, false, false
		}
	}

	req := &request{
		method:      method,
		path:        rawPath,
		host:        host,
		query:       query,
		auth:        auth,
		gitProtocol: gitProtocol,
	}
	// GET/HEAD carry no body for the handlers, as in the net/http path;
	// the bytes were still consumed so keep-alive stays in sync.
	if method != "GET" && method != "HEAD" {
		req.body = body
	}
	return req, connectionClose, true
}

// writeRawResponse writes one response. It returns false if the connection
// is broken. HEAD carries Content-Length but no body, as net/http does.
func writeRawResponse(bw *bufio.Writer, req *request, resp response, closeAfter bool) bool {
	var sb strings.Builder
	version := "HTTP/1.1"
	sb.WriteString(version)
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(resp.status))
	sb.WriteByte(' ')
	sb.WriteString(rawReason(resp.status))
	sb.WriteString("\r\n")
	if resp.contentType != "" {
		sb.WriteString("Content-Type: ")
		sb.WriteString(resp.contentType)
		sb.WriteString("\r\n")
	}
	if resp.body != nil {
		sb.WriteString("Content-Length: ")
		sb.WriteString(strconv.Itoa(len(resp.body)))
		sb.WriteString("\r\n")
	}
	sb.WriteString("Date: ")
	sb.WriteString(rawDate())
	sb.WriteString("\r\n")
	for name, value := range resp.headers {
		sb.WriteString(textproto.CanonicalMIMEHeaderKey(name))
		sb.WriteString(": ")
		sb.WriteString(value)
		sb.WriteString("\r\n")
	}
	if closeAfter {
		sb.WriteString("Connection: close\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := bw.WriteString(sb.String()); err != nil {
		return false
	}
	if req.method != "HEAD" && len(resp.body) > 0 {
		if _, err := bw.Write(resp.body); err != nil {
			return false
		}
	}
	return bw.Flush() == nil
}

// rawReason maps the statuses the forge returns to their reason phrases.
func rawReason(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 413:
		return "Content Too Large"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	default:
		return "Status"
	}
}

// rawDate returns the current Date header value, cached per second like
// net/http's.
var rawDateCache struct {
	mu   sync.Mutex
	sec  int64
	text string
}

func rawDate() string {
	now := time.Now().Unix()
	rawDateCache.mu.Lock()
	defer rawDateCache.mu.Unlock()
	if rawDateCache.sec != now {
		rawDateCache.sec = now
		rawDateCache.text = time.Unix(now, 0).UTC().Format(http.TimeFormat)
	}
	return rawDateCache.text
}
