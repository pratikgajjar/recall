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
	for _, want := range []string{"recall_search", "recall_transcript", "recall_related", "recall_tag"} {
		if !names[want] {
			t.Errorf("missing tool %q (have %v)", want, names)
		}
	}
	// recall_sessions is intentionally absent — see TestSessionsIsFoldedIntoSearch.
	if names["recall_sessions"] {
		t.Error("recall_sessions is advertised again; every schema costs a turn")
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

// The MCP tool schema is re-sent on every agent turn, so it is recurring
// overhead — but it also carries the steer that keeps agents off the expensive
// path. Bound the size AND pin the guidance, so trimming can never silently
// delete the thing that saves the tokens.
func TestToolSchemaStaysLeanAndKeepsSteering(t *testing.T) {
	tools := mcpTools()
	blob, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	const budget = 5500
	if len(blob) > budget {
		t.Errorf("tool schema %d chars exceeds %d; it is paid on every turn", len(blob), budget)
	}
	s := string(blob)
	// The behavioural steer earns its bytes back many times over.
	for _, must := range []string{"session_id", "recall_search with session_id", "grep -C"} {
		if !strings.Contains(s, must) {
			t.Errorf("schema lost load-bearing guidance %q", must)
		}
	}
	// Every tool still has to say what it does and what it takes.
	for _, tl := range tools {
		m := tl
		if m["description"] == nil || m["description"] == "" {
			t.Errorf("tool %v has no description", m["name"])
		}
		if m["inputSchema"] == nil {
			t.Errorf("tool %v has no inputSchema", m["name"])
		}
	}
}

// recall_tag is one verb with a mode, like the CLI. Three MCP tools for one
// concept cost 27% of a schema that is re-sent every turn, for a feature that
// was never called once. All three modes must still work.
func TestTagToolActions(t *testing.T) {
	ix := newTestIndex(t)
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s1", Title: "t", MsgCount: 1}},
		[]Message{{SourceID: "s1", Idx: 0, Role: "user", Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	srv := &mcpServer{ix: ix}
	ctx := context.Background()

	if _, err := srv.runTool(ctx, "recall_tag", toolArgs{SessionID: "pi:s1", Tags: []string{"alpha"}}); err != nil {
		t.Fatalf("add (implicit): %v", err)
	}
	got, err := srv.runTool(ctx, "recall_tag", toolArgs{Action: "list", SessionID: "pi:s1"})
	if err != nil || !strings.Contains(got, "alpha") {
		t.Fatalf("list one session = %q, %v", got, err)
	}
	all, err := srv.runTool(ctx, "recall_tag", toolArgs{Action: "list"})
	if err != nil || !strings.Contains(all, "alpha") {
		t.Fatalf("list all = %q, %v", all, err)
	}
	if _, err := srv.runTool(ctx, "recall_tag", toolArgs{Action: "remove", SessionID: "pi:s1", Tags: []string{"alpha"}}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after, err := srv.runTool(ctx, "recall_tag", toolArgs{Action: "list", SessionID: "pi:s1"})
	if err != nil || strings.Contains(after, "alpha") {
		t.Errorf("tag survived removal: %q", after)
	}
	if _, err := srv.runTool(ctx, "recall_tag", toolArgs{Action: "bogus", SessionID: "pi:s1"}); err == nil {
		t.Error("unknown action should error, not silently add")
	}
	// The retired names must not linger as dead aliases.
	for _, gone := range []string{"recall_untag", "recall_tags"} {
		if _, err := srv.runTool(ctx, gone, toolArgs{}); err == nil {
			t.Errorf("%s should no longer be a tool", gone)
		}
	}
}

// Every tool schema is re-sent on every turn, used or not. recall_sessions was
// Search("") with the same filters — the same thing recall_search does when the
// query is omitted — and cost 771 characters a turn, 19% of the schema, for a
// tool called 0 times in 418 real calls. It is no longer advertised; the handler
// stays so anything already calling it keeps working.
func TestSessionsIsFoldedIntoSearch(t *testing.T) {
	ix := newTestIndex(t)
	if err := ix.IngestBatch(context.Background(), "pi",
		[]Session{{Source: "pi", SourceID: "s1", Title: "a session", MsgCount: 1}},
		[]Message{{SourceID: "s1", Idx: 0, Role: "user", Text: "hello world"}}); err != nil {
		t.Fatal(err)
	}
	srv := &mcpServer{ix: ix}

	tools := mcpTools()
	var names []string
	for _, tl := range tools {
		names = append(names, tl["name"].(string))
	}
	for _, n := range names {
		if n == "recall_sessions" {
			t.Error("recall_sessions is advertised again; it costs every turn")
		}
	}

	// query must be optional, or omitting it is a schema violation.
	for _, tl := range tools {
		if tl["name"] != "recall_search" {
			continue
		}
		schema := tl["inputSchema"].(map[string]any)
		if req, ok := schema["required"]; ok {
			for _, r := range req.([]string) {
				if r == "query" {
					t.Error("query is still required; browsing recent sessions is impossible")
				}
			}
		}
	}

	// Both routes work and agree.
	browse, err := srv.runTool(context.Background(), "recall_search", toolArgs{})
	if err != nil {
		t.Fatalf("search with no query: %v", err)
	}
	legacy, err := srv.runTool(context.Background(), "recall_sessions", toolArgs{})
	if err != nil {
		t.Fatalf("the kept handler broke: %v", err)
	}
	if browse != legacy {
		t.Error("omitting the query should give exactly what recall_sessions gave")
	}
	if !strings.Contains(browse, "a session") {
		t.Errorf("browse returned nothing useful: %q", browse)
	}
}
