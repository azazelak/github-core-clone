// Hand-rolled HTTP/1.1 server on net.Listener: goroutine per connection,
// keep-alive request loop, Content-Length and chunked request bodies,
// Expect: 100-continue. The dispatcher builds the whole response under
// the store lock and the connection loop writes it after unlocking.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// reqSeq hands out sequential X-GitHub-Request-Id values without taking
// the store mutex (request ids are wire metadata, not recorded state).
var reqSeq atomic.Int64

type req struct {
	method  string
	rawPath string // path as sent, percent-encoded
	path    string // raw path (segments decoded at use sites, like v1)
	query   url.Values
	hdrs    [24][2]string // header name/value pairs, raw case
	nHdrs   int
	xHdrs   [][2]string // overflow when a request carries >24 headers
	host    string
	rawTgt  string
	body    []byte
	close   bool
}

// header returns the last value sent for name (case-insensitive), or "".
func (q *req) header(name string) string {
	v := ""
	for i := 0; i < q.nHdrs; i++ {
		if strings.EqualFold(q.hdrs[i][0], name) {
			v = q.hdrs[i][1]
		}
	}
	for _, h := range q.xHdrs {
		if strings.EqualFold(h[0], name) {
			v = h[1]
		}
	}
	return v
}

func (q *req) addHeader(name, value string) {
	if q.nHdrs < len(q.hdrs) {
		q.hdrs[q.nHdrs] = [2]string{name, value}
		q.nHdrs++
	} else {
		q.xHdrs = append(q.xHdrs, [2]string{name, value})
	}
}

type resp struct {
	status int
	ctype  string
	extra  map[string]string
	body   []byte
}

func jbytes(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

func jsonResp(status int, v any, extra map[string]string) *resp {
	return &resp{status: status, ctype: jsonType, extra: extra, body: jbytes(v)}
}

func errResp(status int, msg string) *resp {
	return jsonResp(status, map[string]any{"message": msg}, nil)
}

var readerPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 8192) }}
var writerPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, 8192) }}

var statusText = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func writeResp(w *bufio.Writer, r *req, rs *resp) {
	text, ok := statusText[rs.status]
	if !ok {
		text = "OK"
	}
	var num [32]byte
	w.WriteString("HTTP/1.1 ")
	w.Write(strconv.AppendInt(num[:0], int64(rs.status), 10))
	w.WriteString(" ")
	w.WriteString(text)
	w.WriteString("\r\n")
	if rs.ctype != "" {
		w.WriteString("content-type: ")
		w.WriteString(rs.ctype)
		w.WriteString("\r\n")
	}
	w.WriteString("content-length: ")
	w.Write(strconv.AppendInt(num[:0], int64(len(rs.body)), 10))
	w.WriteString("\r\nx-github-request-id: req-")
	w.Write(strconv.AppendInt(num[:0], reqSeq.Add(1), 10))
	w.WriteString("\r\n")
	for k, v := range rs.extra {
		w.WriteString(k)
		w.WriteString(": ")
		w.WriteString(v)
		w.WriteString("\r\n")
	}
	if r.close {
		w.WriteString("connection: close\r\n")
	}
	w.WriteString("\r\n")
	if len(rs.body) > 0 {
		w.Write(rs.body)
	}
	w.Flush()
}

func readChunkedBody(br *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(line, 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			// trailer headers until blank line
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if l == "\r\n" || l == "\n" {
					break
				}
			}
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, n); err != nil {
			return nil, err
		}
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

func readRequest(br *bufio.Reader, bw *bufio.Writer) (*req, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, errBadRequest
	}
	rq := &req{method: parts[0], rawTgt: parts[1]}
	target := parts[1]
	// Fast path (the overwhelmingly common case: origin-form target with
	// no query string and no percent-escapes): skip url.Parse entirely.
	if strings.HasPrefix(target, "/") && !strings.ContainsAny(target, "?%") {
		rq.rawPath = target
		rq.path = target
		rq.query = url.Values{}
	} else if u, err := url.Parse(target); err == nil {
		rq.rawPath = u.EscapedPath()
		if rq.rawPath == "" {
			rq.rawPath = u.Path
		}
		rq.path = rq.rawPath
		rq.query = u.Query()
	} else {
		rq.rawPath = target
		rq.path = target
		rq.query = url.Values{}
	}
	for {
		hline, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if hline == "\r\n" || hline == "\n" {
			break
		}
		hline = strings.TrimRight(hline, "\r\n")
		if i := strings.IndexByte(hline, ':'); i > 0 {
			rq.addHeader(strings.TrimSpace(hline[:i]), strings.TrimSpace(hline[i+1:]))
		}
	}
	rq.host = rq.header("host")
	if rq.header("connection") == "close" {
		rq.close = true
	}
	if strings.EqualFold(rq.header("expect"), "100-continue") {
		// Answer before reading the body: a client (git/curl) waits for
		// the 100 before it sends the body.
		bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
		bw.Flush()
	}
	if rq.method == "GET" || rq.method == "HEAD" {
		return rq, nil
	}
	if strings.Contains(rq.header("transfer-encoding"), "chunked") {
		body, err := readChunkedBody(br)
		if err != nil {
			return nil, err
		}
		rq.body = body
		return rq, nil
	}
	if cl := rq.header("content-length"); cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 || n > 256<<20 {
			return nil, errBadRequest
		}
		if n == 0 {
			return rq, nil
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, err
		}
		rq.body = body
	}
	return rq, nil
}

var errBadRequest = &httpError{"bad request"}

type httpError struct{ s string }

func (e *httpError) Error() string { return e.s }

func handleConn(conn net.Conn) {
	defer conn.Close()
	br := readerPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer readerPool.Put(br)
	bw := writerPool.Get().(*bufio.Writer)
	bw.Reset(conn)
	defer writerPool.Put(bw)
	for {
		rq, err := readRequest(br, bw)
		if err != nil {
			return
		}
		rs := dispatch(rq)
		writeResp(bw, rq, rs)
		if rq.close {
			return
		}
	}
}

func serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(conn)
	}
}

// ---------------------------------------------------------------- dispatcher

func authed(rq *req) bool {
	if token == "" {
		return true
	}
	return rq.header("authorization") == "Bearer "+token
}

func gitRepoFromPath(path string) (owner, name, suffix string, ok bool) {
	if !strings.HasPrefix(path, "/") {
		return "", "", "", false
	}
	rest := path[1:]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", "", false
	}
	owner = rest[:i]
	rest = rest[i+1:]
	j := strings.Index(rest, ".git")
	if j < 0 {
		return "", "", "", false
	}
	name = rest[:j]
	suffix = rest[j+4:]
	if suffix != "" && !strings.HasPrefix(suffix, "/") {
		return "", "", "", false
	}
	if owner == "" || name == "" {
		return "", "", "", false
	}
	return owner, name, suffix, true
}

func dispatch(rq *req) (rs *resp) {
	defer func() {
		if recover() != nil {
			rs = errResp(500, "Internal Server Error")
		}
	}()
	st.mu.Lock()
	defer st.mu.Unlock()
	path := rq.path
	method := rq.method
	if method == "GET" && path == "/healthz" {
		return jsonResp(200, map[string]any{"ok": true}, nil)
	}
	if path == "/api/graphql" && method == "POST" {
		return handleGraphQL(rq)
	}
	if path == "/mcp" && method == "POST" {
		return handleMCP(rq)
	}
	if path == "/mcp" && method == "DELETE" {
		return &resp{status: 200, body: []byte{}}
	}
	if strings.HasPrefix(path, "/api/v3") {
		return handleRest(rq)
	}
	if owner, name, suffix, ok := gitRepoFromPath(path); ok {
		owner = unescape(owner)
		name = unescape(name)
		r := st.findRepo(owner, name)
		if method == "GET" && strings.HasPrefix(suffix, "/info/refs") {
			if r == nil {
				return errResp(404, "Not Found")
			}
			service := rq.query.Get("service")
			v2 := rq.header("git-protocol") == "version=2" && service == "git-upload-pack"
			var body []byte
			var ct string
			switch {
			case service == "git-receive-pack":
				body = v0Advertise(r, "git-receive-pack")
				ct = "application/x-git-receive-pack-advertisement"
			case v2:
				body = v2Advertise()
				ct = "application/x-git-upload-pack-advertisement"
			default:
				body = v0Advertise(r, "git-upload-pack")
				ct = "application/x-git-upload-pack-advertisement"
			}
			return &resp{status: 200, ctype: ct, body: body}
		}
		if method == "POST" && suffix == "/git-upload-pack" {
			if r == nil {
				return errResp(404, "Not Found")
			}
			var out []byte
			func() {
				defer func() {
					if recover() != nil {
						out = append(pktStr("NAK\n"), flushPkt...)
					}
				}()
				if rq.header("git-protocol") == "version=2" {
					out = handleFetchV2(r, rq.body)
				} else {
					out = handleFetchV0(r, rq.body)
				}
			}()
			return &resp{status: 200, ctype: "application/x-git-upload-pack-result", body: out}
		}
		if method == "POST" && suffix == "/git-receive-pack" {
			if r == nil {
				return errResp(404, "Not Found")
			}
			var out []byte
			func() {
				defer func() {
					if recover() != nil {
						report := append(pktStr("unpack error\n"), flushPkt...)
						chunk := append([]byte{1}, report...)
						out = append(pktLine(chunk), flushPkt...)
					}
				}()
				out = handleReceivePack(r, rq.body)
			}()
			return &resp{status: 200, ctype: "application/x-git-receive-pack-result", body: out}
		}
		return errResp(404, "Not Found")
	}
	return errResp(404, "Not Found")
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}
