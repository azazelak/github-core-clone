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
)

type req struct {
	method  string
	rawPath string // path as sent, percent-encoded
	path    string // raw path (segments decoded at use sites, like v1)
	query   url.Values
	headers map[string]string
	host    string
	rawTgt  string
	body    []byte
	close   bool
}

func (q *req) header(name string) string { return q.headers[strings.ToLower(name)] }

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
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 " + strconv.Itoa(rs.status) + " " + text + "\r\n")
	if rs.ctype != "" {
		b.WriteString("content-type: " + rs.ctype + "\r\n")
	}
	b.WriteString("content-length: " + strconv.Itoa(len(rs.body)) + "\r\n")
	st.mu.Lock()
	b.WriteString("x-github-request-id: " + st.nextReqID() + "\r\n")
	st.mu.Unlock()
	for k, v := range rs.extra {
		b.WriteString(k + ": " + v + "\r\n")
	}
	if r.close {
		b.WriteString("connection: close\r\n")
	}
	b.WriteString("\r\n")
	w.Write(b.Bytes())
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
	rq := &req{method: parts[0], rawTgt: parts[1], headers: make(map[string]string)}
	target := parts[1]
	if u, err := url.Parse(target); err == nil {
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
			rq.headers[strings.ToLower(strings.TrimSpace(hline[:i]))] = strings.TrimSpace(hline[i+1:])
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
