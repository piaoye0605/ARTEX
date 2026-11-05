package mcphttp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// fakeHTTP is a Streamable-HTTP MCP server: initialize assigns a session id,
// tools/list answers JSON, tools/call can answer either JSON or SSE. Every
// request's interesting headers are recorded for assertions.
type fakeHTTP struct {
	srv      *httptest.Server
	mu       sync.Mutex
	sid      string
	sseCall  bool
	deleted  int
	deleted2 string // last Mcp-Session-Id seen on DELETE
	headers  []map[string]string
}

func newFakeHTTP(t *testing.T) *fakeHTTP {
	t.Helper()
	f := &fakeHTTP{sid: fmt.Sprintf("sid-%d", time.Now().UnixNano())}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rec := map[string]string{
			"accept": r.Header.Get("Accept"),
			"proto":  r.Header.Get("MCP-Protocol-Version"),
			"auth":   r.Header.Get("Authorization"),
			"sid":    r.Header.Get("Mcp-Session-Id"),
		}
		f.mu.Lock()
		f.headers = append(f.headers, rec)
		f.mu.Unlock()

		if r.Method == http.MethodDelete {
			f.mu.Lock()
			f.deleted++
			f.deleted2 = r.Header.Get("Mcp-Session-Id")
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == 0 { // notification — no body expected
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var result any
		sse := false
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
			w.Header().Set("Mcp-Session-Id", f.sid)
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "scan", "description": "run a scan", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				map[string]any{"name": "ping", "description": "ping the target"},
			}}
		case "tools/call":
			if func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.sseCall }() {
				sse = true
			}
			result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "part1-"}, map[string]any{"type": "text", "text": "part2"}},
			}
		default:
			result = map[string]any{}
		}
		payload, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: mustJSON(result)})
		if sse {
			// Stream the reply as SSE: first an unrelated frame (a server-initiated
			// notification with no id), then a response for the WRONG id, then the
			// real one. parseSSE must skip both and match on id.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"progress\",\"params\":{}}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[]}}\n\n", req.ID+1000)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func (f *fakeHTTP) header(i int) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.headers) {
		return nil
	}
	return f.headers[i]
}

func TestStreamableHTTPConnectSessionAndHeaders(t *testing.T) {
	f := newFakeHTTP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New(ctx, "fake", f.srv.URL, map[string]string{" Authorization ": "Bearer tok"}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// initialize was request #1: both content types accepted, protocol version
	// declared, custom headers forwarded (with the key trimmed).
	h := f.header(0)
	if h == nil || h["accept"] != "application/json, text/event-stream" {
		t.Fatalf("initialize accept=%q", h["accept"])
	}
	if h["proto"] != protocolVersion {
		t.Fatalf("initialize protocol=%q", h["proto"])
	}
	if h["auth"] != "Bearer tok" {
		t.Fatalf("initialize auth=%q", h["auth"])
	}

	// The session id from the initialize response is replayed on later calls.
	tools, err := c.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if h := f.header(2); h == nil || h["sid"] != f.sid {
		t.Fatalf("tools/list did not carry session id: %+v", h)
	}
	if len(tools) != 2 {
		t.Fatalf("tools=%d want 2", len(tools))
	}
	if tools[0].Name() != "mcp__fake__scan" {
		t.Fatalf("tool name=%q", tools[0].Name())
	}
	// A tool without inputSchema gets the default object schema.
	if schema := tools[1].InputSchema(); schema == nil || schema["type"] != "object" {
		t.Fatalf("default schema missing: %#v", schema)
	}

	// Close sends DELETE with the session id (best-effort session teardown).
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.mu.Lock()
	deleted, sid := f.deleted, f.deleted2
	f.mu.Unlock()
	if deleted != 1 || sid != f.sid {
		t.Fatalf("DELETE seen=%d sid=%q want 1/%s", deleted, sid, f.sid)
	}
}

// The wrapped CoreTool concatenates text blocks and forwards isError; the
// backend Call() path shares the same behaviour.
func TestStreamableHTTPToolCall(t *testing.T) {
	f := newFakeHTTP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := New(ctx, "fake", f.srv.URL, nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools, err := c.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	res, err := tools[0].Call(ctx, json.RawMessage(`{"target":"x"}`), &actool.ToolContext{MaxOutputChars: 30000})
	if err != nil {
		t.Fatalf("wrapped Call: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "part1-part2" {
		t.Fatalf("wrapped result=%+v", res)
	}

	got, err := c.Call(ctx, "scan", map[string]any{"target": "x"})
	if err != nil || got != "part1-part2" {
		t.Fatalf("Call=%q err=%v", got, err)
	}

}

func TestCallIsErrorAndBadPayload(t *testing.T) {
	var mode int32 // 1 = isError, 2 = garbage result, 3 = rpc error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == 0 || req.Method != "tools/call" {
			if req.ID == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(mustJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: mustJSON(map[string]any{})}))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case 1:
			w.Write(mustJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: mustJSON(map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "boom"}},
				"isError": true,
			})}))
		case 2:
			w.Write(mustJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`123`)}))
		case 3:
			w.Write(mustJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32000, Message: "denied"}}))
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := New(ctx, "fake", srv.URL, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	mode = 1
	if _, err := c.Call(ctx, "scan", nil); err == nil || !strings.Contains(err.Error(), `mcp tool "scan" error`) {
		t.Fatalf("isError path: %v", err)
	}
	mode = 2
	if _, err := c.Call(ctx, "scan", nil); err == nil || !strings.Contains(err.Error(), "bad MCP response") {
		t.Fatalf("garbage result path: %v", err)
	}
	mode = 3
	if _, err := c.Call(ctx, "scan", nil); err == nil || !strings.Contains(err.Error(), "mcp rpc error -32000") {
		t.Fatalf("rpc error path: %v", err)
	}
}

func TestNewRejectsBadHeaders(t *testing.T) {
	ctx := context.Background()
	for _, headers := range []map[string]string{
		{"": "x"},
		{"bad\r\nheader": "x"},
		{"X\nY": "x"},
	} {
		if _, err := New(ctx, "fake", "http://127.0.0.1:1", headers, false); err == nil ||
			!strings.Contains(err.Error(), "invalid HTTP header name") {
			t.Fatalf("headers=%v accepted: %v", headers, err)
		}
	}
}

func TestHTTPStatusErrorCarriesSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := New(context.Background(), "fake", srv.URL, nil, false)
	if err == nil || !strings.Contains(err.Error(), "mcp http 500") || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("err=%v", err)
	}
}

// The server may answer a POST with an SSE stream instead of JSON. The client
// must pick the frame whose id matches and skip notifications and foreign ids.
func TestStreamableHTTPSSEResponse(t *testing.T) {
	f := newFakeHTTP(t)
	f.mu.Lock()
	f.sseCall = true
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New(ctx, "fake", f.srv.URL, nil, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Call(ctx, "scan", nil)
	if err != nil || got != "part1-part2" {
		t.Fatalf("SSE Call=%q err=%v", got, err)
	}
}

func TestParseSSEUnits(t *testing.T) {
	stream := strings.Join([]string{
		`data: not-json`,
		``,
		`data: {"jsonrpc":"2.0","method":"s2c-request","params":{}}`,
		``,
		`data: {"jsonrpc":"2.0","id":42,"result":{"x":1}}`,
		``,
		`data: {"jsonrpc":"2.0","id":7,"result":{"y":2}}`,
		``,
	}, "\n")
	resp, err := parseSSE(strings.NewReader(stream), 7)
	if err != nil || resp.ID != 7 {
		t.Fatalf("parseSSE=%+v err=%v want id 7", resp, err)
	}

	if _, err := parseSSE(strings.NewReader(`data: {"jsonrpc":"2.0","id":1}`), 9); err == nil ||
		!strings.Contains(err.Error(), "no matching response") {
		t.Fatalf("missing-id err=%v", err)
	}

	// A frame arriving without the closing blank line is still flushed at EOF.
	resp, err = parseSSE(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}"), 3)
	if err != nil || resp.ID != 3 {
		t.Fatalf("eof flush=%+v err=%v", resp, err)
	}
}

func TestReadSSEEndpoint(t *testing.T) {
	// Relative endpoint resolves against the SSE URL; absolute is kept as-is;
	// non-data lines are skipped.
	got, err := readSSEEndpoint(bufio.NewReader(strings.NewReader("retry: 1000\nevent: endpoint\ndata: /message?sid=1\n\n")), "http://h/api/sse")
	if err != nil || got != "http://h/message?sid=1" {
		t.Fatalf("relative=%q err=%v", got, err)
	}
	got, err = readSSEEndpoint(bufio.NewReader(strings.NewReader("data: https://other.example/mcp\n")), "http://h/sse")
	if err != nil || got != "https://other.example/mcp" {
		t.Fatalf("absolute=%q err=%v", got, err)
	}
	// An empty data line is ignored; the next one wins.
	got, err = readSSEEndpoint(bufio.NewReader(strings.NewReader("data:\ndata: /m\n")), "http://h/sse")
	if err != nil || got != "http://h/m" {
		t.Fatalf("blank=%q err=%v", got, err)
	}
	if _, err := readSSEEndpoint(bufio.NewReader(strings.NewReader("event: x\n")), "http://h/sse"); err == nil {
		t.Fatal("EOF must be an error")
	}
}

// When the SSE stream dies while a call is in flight, the pending call fails
// with a stream error instead of hanging.
func TestLegacySSEStreamError(t *testing.T) {
	var mu sync.Mutex
	var stream http.ResponseWriter
	var flusher http.Flusher
	closeStream := make(chan struct{})
	messages := make(chan []byte, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			mu.Lock()
			stream, flusher = w, w.(http.Flusher)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: endpoint\ndata: %s/message\n\n", "http://"+r.Host)
			flusher.Flush()
			for {
				select {
				case payload := <-messages:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
					flusher.Flush()
				case <-closeStream:
					return // kill the stream mid-session
				case <-r.Context().Done():
					return
				}
			}
		case "/message":
			var req rpcRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			result, _ := json.Marshal(map[string]any{"protocolVersion": legacySSEProtocolVersion})
			payload, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
			// Deliver on the SSE stream, like a real legacy server would.
			mu.Lock()
			ok := stream != nil
			mu.Unlock()
			if ok {
				messages <- payload
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewSSE(ctx, "gsl", srv.URL+"/sse", nil, false)
	if err != nil {
		t.Fatalf("NewSSE: %v", err)
	}
	defer c.Close()

	// Kill the stream and wait for the reader goroutine to observe the EOF —
	// that is what feeds the streamErr channel the pending call selects on.
	mu.Lock()
	ready := stream != nil
	mu.Unlock()
	if !ready {
		t.Fatal("stream handler never started")
	}
	close(closeStream)
	select {
	case <-c.streamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("reader never noticed the closed stream")
	}

	if _, err := c.Call(ctx, "echo", nil); err == nil || !strings.Contains(err.Error(), "mcp sse stream") {
		t.Fatalf("stream error not surfaced: %v", err)
	}
}
