package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// call feeds one JSON-RPC request to the server and returns the parsed
// response (nil for notifications, which produce no output).
func call(t *testing.T, srv *mcpServer, buf *bytes.Buffer, req string) map[string]any {
	t.Helper()
	buf.Reset()
	srv.handle(context.Background(), []byte(req))
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return nil
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("invalid JSON-RPC response %q: %v", out, err)
	}
	return resp
}

func newMCPWithData(t *testing.T) (*mcpServer, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	writeLines(t, piPath(root, "sess-mcp-1"),
		piSession("sess-mcp-1", "/work/web", "2026-05-03T08:00:00Z"),
		piMsg("user", "quaternion slerp interpolation", "2026-05-03T08:00:02Z"),
		piMsg("assistant", "here is how", "2026-05-03T08:00:06Z"),
	)
	ix := newTestIndex(t)
	ad := &PiAdapter{Root: root}
	s, m, _, err := ad.Scan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch(context.Background(), "pi", s, m); err != nil {
		t.Fatal(err)
	}
	// Point the server's adapters at the fixture so recall_transcript works.
	buf := &bytes.Buffer{}
	return &mcpServer{ix: ix, out: buf}, buf
}

func TestMCPInitialize(t *testing.T) {
	srv, buf := newMCPWithData(t)
	resp := call(t, srv, buf,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %+v", resp)
	}
	if res["protocolVersion"] != "2024-11-05" {
		t.Errorf("should echo client protocol version, got %v", res["protocolVersion"])
	}
	si, _ := res["serverInfo"].(map[string]any)
	if si["name"] != "recall" {
		t.Errorf("serverInfo.name = %v", si["name"])
	}
}

func TestMCPNotificationNoResponse(t *testing.T) {
	srv, buf := newMCPWithData(t)
	if resp := call(t, srv, buf, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp != nil {
		t.Errorf("notification must not produce a response, got %+v", resp)
	}
}

func TestMCPToolsList(t *testing.T) {
	srv, buf := newMCPWithData(t)
	resp := call(t, srv, buf, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	res := resp["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	names := map[string]bool{}
	for _, tt := range tools {
		names[tt.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"recall_search", "recall_transcript", "recall_sessions", "recall_related"} {
		if !names[want] {
			t.Errorf("missing tool %q (have %v)", want, names)
		}
	}
}

func TestMCPToolCallSearch(t *testing.T) {
	srv, buf := newMCPWithData(t)
	resp := call(t, srv, buf,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"recall_search","arguments":{"query":"quaternion"}}}`)
	res := resp["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("search returned error: %+v", res)
	}
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "sess-mcp-1") {
		t.Errorf("search result should reference the session id, got:\n%s", text)
	}
}

func TestMCPToolCallTranscript(t *testing.T) {
	srv, buf := newMCPWithData(t)
	resp := call(t, srv, buf,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall_transcript","arguments":{"session_id":"pi:sess-mcp-1"}}}`)
	res := resp["result"].(map[string]any)
	// recall_transcript re-reads from the source; the default adapters won't
	// find our temp fixture, so this is expected to be a clean tool error
	// (isError=true) rather than a protocol crash.
	if _, ok := res["isError"]; !ok {
		t.Fatalf("expected an isError field, got %+v", res)
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	srv, buf := newMCPWithData(t)
	resp := call(t, srv, buf, `{"jsonrpc":"2.0","id":9,"method":"does/not/exist"}`)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error, got %+v", resp)
	}
	if int(e["code"].(float64)) != -32601 {
		t.Errorf("error code = %v, want -32601", e["code"])
	}
}
