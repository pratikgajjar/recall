package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

// recall mcp — a Model Context Protocol server over stdio. Works with any
// MCP-capable harness (Claude Code, Codex, Cursor, Cline, …). It reuses the
// same local index the CLI builds and keeps it warm in the background, so
// tool calls read an already-fresh index.

const mcpProtocolVersion = "2025-06-18"

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	noRefresh := fs.Bool("no-refresh", false, "do not refresh the index in the background")
	interval := fs.Duration("refresh-interval", 30*time.Second, "background index refresh interval")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ix, err := openIndex(defaultIndexPath())
	if err != nil {
		return err
	}
	defer ix.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if !*noRefresh {
		go func() {
			_ = refreshIndex(ctx, ix) // catch up at startup
			t := time.NewTicker(*interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_ = refreshIndex(ctx, ix)
				}
			}
		}()
	}

	srv := &mcpServer{ix: ix, out: os.Stdout}
	return srv.serve(ctx, os.Stdin)
}

// refreshIndex runs one silent incremental pass over every available adapter.
// It writes nothing to stdout (reserved for the MCP protocol); errors per
// source are swallowed so a single bad source can't stall the server.
func refreshIndex(ctx context.Context, ix *Index) error {
	for _, ad := range defaultAdapters() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !ad.Available() {
			continue
		}
		prev := ix.GetMeta("ckpt:" + ad.ID())
		sessions, msgs, next, err := ad.Scan(ctx, prev)
		if err != nil {
			continue
		}
		if err := ix.IngestBatch(ctx, ad.ID(), sessions, msgs); err != nil {
			continue
		}
		if next != "" {
			_ = ix.SetMeta("ckpt:"+ad.ID(), next)
		}
	}
	_ = ix.SetMeta("indexed_at", time.Now().Format(time.RFC3339))
	return nil
}

// --- JSON-RPC 2.0 over newline-delimited stdio ----------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpServer struct {
	ix  *Index
	out io.Writer
	mu  sync.Mutex
}

func (s *mcpServer) serve(ctx context.Context, in io.Reader) error {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.handle(ctx, line)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *mcpServer) handle(ctx context.Context, raw []byte) {
	var req rpcRequest
	if err := JSONUnmarshal(raw, &req); err != nil {
		return
	}
	isNotification := len(req.ID) == 0
	result, rerr := s.dispatch(ctx, req)
	if isNotification {
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	s.write(resp)
}

func (s *mcpServer) write(resp rpcResponse) {
	b, err := JSONMarshal(resp)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(b)
	_, _ = s.out.Write([]byte{'\n'})
}

func (s *mcpServer) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		return s.toolsCall(ctx, req.Params)
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func (s *mcpServer) initialize(params json.RawMessage) any {
	pv := mcpProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && JSONUnmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		pv = p.ProtocolVersion // echo the client's version when it sends one
	}
	return map[string]any{
		"protocolVersion": pv,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "recall", "version": version},
		"instructions": "Search the user's own past AI chat history across Cursor, Claude Code, " +
			"Codex, and pi. When the user references earlier work ('how did we fix…', 'continue the…'), " +
			"call recall_search, then recall_transcript with a returned session id to read it in full.",
	}
}

// --- tools -----------------------------------------------------------------

type toolArgs struct {
	Query     string   `json:"query"`
	SessionID string   `json:"session_id"`
	Repo      string   `json:"repo"`
	Source    string   `json:"source"`
	Since     string   `json:"since"`
	After     string   `json:"after"`
	Before    string   `json:"before"`
	Limit     int      `json:"limit"`
	Range     string   `json:"range"`   // Python-style slice over the message list
	Outline   bool     `json:"outline"` // outline mode (one line per message)
	Role      string   `json:"role"`    // comma-separated roles: user,assistant,tool
	Tags      []string `json:"tags"`    // tag filter (search) or tags to add/remove
}

func (a toolArgs) opts(defLimit int) (SearchOpts, error) {
	// Tags may carry a reserved facet (e.g. "source:cursor"); split it out. An
	// explicit `source` arg still wins if both are given.
	tags, facetSource := parseSelectors(a.Tags)
	source := a.Source
	if source == "" {
		source = facetSource
	}
	opts := SearchOpts{Source: source, Project: resolveRepo(a.Repo), Limit: a.Limit, Tags: tags, SessionID: a.SessionID}
	if opts.Limit <= 0 {
		opts.Limit = defLimit
	}
	if a.Repo == "." {
		if cwd, err := os.Getwd(); err == nil {
			opts.Project = resolveRepo(cwd)
		}
	}
	lower := a.After
	if lower == "" {
		lower = a.Since
	}
	if lower != "" {
		t, err := parseInstant(lower)
		if err != nil {
			return opts, err
		}
		opts.After = t
	}
	if a.Before != "" {
		t, err := parseInstant(a.Before)
		if err != nil {
			return opts, err
		}
		opts.Before = t
	}
	return opts, nil
}

func (s *mcpServer) toolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string   `json:"name"`
		Arguments toolArgs `json:"arguments"`
	}
	if err := JSONUnmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	text, err := s.runTool(ctx, call.Name, call.Arguments)
	if err != nil {
		return mcpText(err.Error(), true), nil
	}
	return mcpText(text, false), nil
}

func (s *mcpServer) runTool(ctx context.Context, name string, a toolArgs) (string, error) {
	switch name {
	case "recall_search":
		opts, err := a.opts(15)
		if err != nil {
			return "", err
		}
		if err := resolveSessionDot(s.ix, &opts); err != nil {
			return "", err
		}
		hits, err := s.ix.Search(a.Query, opts)
		if err != nil {
			return "", err
		}
		return hitsText(hits), nil
	case "recall_sessions":
		opts, err := a.opts(20)
		if err != nil {
			return "", err
		}
		hits, err := s.ix.Search("", opts)
		if err != nil {
			return "", err
		}
		return hitsText(hits), nil
	case "recall_related":
		limit := a.Limit
		if limit <= 0 {
			limit = 10
		}
		hits, err := s.ix.Related(a.SessionID, limit)
		if err != nil {
			return "", err
		}
		return hitsText(hits), nil
	case "recall_transcript":
		return s.transcript(ctx, a)
	case "recall_tag":
		if a.SessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		if len(a.Tags) == 0 {
			return "", fmt.Errorf("tags is required")
		}
		for _, t := range a.Tags {
			if msg := reservedFacetError(t); msg != "" {
				return "", fmt.Errorf("%s", msg)
			}
		}
		added, err := s.ix.AddTags(a.SessionID, a.Tags)
		if err != nil {
			return "", err
		}
		cur, err := s.ix.SessionTags(a.SessionID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("tagged %s (+%d). tags now: %s", a.SessionID, added, strings.Join(cur, " ")), nil
	case "recall_untag":
		if a.SessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		removed, err := s.ix.RemoveTags(a.SessionID, a.Tags)
		if err != nil {
			return "", err
		}
		cur, err := s.ix.SessionTags(a.SessionID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("untagged %s (-%d). tags now: %s", a.SessionID, removed, strings.Join(cur, " ")), nil
	case "recall_tags":
		if a.SessionID != "" {
			tags, err := s.ix.SessionTags(a.SessionID)
			if err != nil {
				return "", err
			}
			return strings.Join(tags, " "), nil
		}
		all, err := s.ix.AllTags()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, tc := range all {
			fmt.Fprintf(&b, "%d\t%s\n", tc.Count, tc.Tag)
		}
		return b.String(), nil
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *mcpServer) transcript(ctx context.Context, a toolArgs) (string, error) {
	id := a.SessionID
	if id == "" {
		opts, err := a.opts(1)
		if err != nil {
			return "", err
		}
		opts.Limit = 1
		if opts.Project == "" {
			if cwd, err := os.Getwd(); err == nil {
				opts.Project = resolveRepo(cwd)
			}
		}
		hits, err := s.ix.Search("", opts)
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "", fmt.Errorf("no sessions found")
		}
		id = hits[0].SessionID
	}
	sess, err := s.ix.LookupSession(id)
	if err != nil {
		return "", fmt.Errorf("session %s: %w", id, err)
	}
	ad := adapterFor(sess.Source)
	if ad == nil {
		return "", fmt.Errorf("no adapter for source %q", sess.Source)
	}
	msgs, err := ad.Fetch(ctx, sess.SourceID)
	if err != nil {
		return "", err
	}
	opts, prelude := applyBigSessionCap(transcriptOpts{Range: a.Range, Outline: a.Outline, Roles: a.Role}, len(msgs))
	var b strings.Builder
	b.WriteString(prelude)
	if err := renderTranscript(&b, sess, msgs, opts); err != nil {
		return "", err
	}
	return b.String(), nil
}

// mcpBigSessionThreshold is when recall_transcript switches to outline-by-default
// for agents that didn't ask for a specific slice.
const mcpBigSessionThreshold = 200

// applyBigSessionCap protects MCP callers from accidentally requesting a
// 30k-message transcript. When no slice was requested and the session is big,
// it switches to outline mode and returns a one-line note explaining how to
// drill in with a 'range' on the next call.
func applyBigSessionCap(opts transcriptOpts, msgCount int) (transcriptOpts, string) {
	// Any explicit slice signal (range / outline / role filter) means the caller
	// knows what they want — don't override.
	if opts.Range != "" || opts.Outline || opts.Roles != "" || msgCount <= mcpBigSessionThreshold {
		return opts, ""
	}
	opts.Outline = true
	return opts, fmt.Sprintf("// session has %d messages; defaulting to outline. Call recall_transcript again with range='FROM:TO' (e.g. range='-50:'), or role='user,assistant' to skip tool noise.\n\n", msgCount)
}

func hitsText(hits []Hit) string {
	var b strings.Builder
	printHits(&b, hits)
	return b.String()
}

func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func mcpTools() []map[string]any {
	repo := strSchema("Restrict to a project folder. Pass '.' for the current working directory.")
	source := strSchema("Restrict to one tool: cursor | claude | codex | pi.")
	since := strSchema("Only sessions newer than this, e.g. '24h', '7d', '30d'.")
	limit := map[string]any{"type": "integer", "description": "Max results."}
	tagsFilter := map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": "Filter selectors, AND-combined. Each is either a user tag (applied with recall_tag) or a reserved facet like 'source:cursor' (cursor|claude|codex|pi). Equivalent to the typed 'source' arg.",
	}

	return []map[string]any{
		{
			"name":        "recall_search",
			"description": "Search your own past AI chat history across Cursor, Claude Code, Codex, and pi. Returns ranked sessions with matched excerpts and a session id. Use recall_transcript to read a hit in full.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":      strSchema("Full-text search over past conversations (titles + message excerpts). Use concrete identifiers, error strings, or feature names."),
					"session_id": strSchema("Restrict the search to one session. Pass '.' for the current/newest session in this repo — useful to find something said earlier in a long session after context compaction."),
					"repo":       repo,
					"source":     source,
					"since":      since,
					"limit":      limit,
					"tags":       tagsFilter,
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "recall_transcript",
			"description": "Read a past AI session as a transcript. Pass a session_id from recall_search, or omit it to get the most recent session (optionally filtered by repo/source/since). Large sessions: use 'outline' for a one-line-per-message overview, 'range' to slice ('305:315' or '-50:'), or 'role' to filter ('user,assistant' skips tool noise; ~50% of long agent loops are tool messages). After a recall_search hit at msg_idx=N, call with range='N-5:N+5' to read around it.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": strSchema("Session id from recall_search/recall_sessions (e.g. 'cursor:…', 'pi:…'). Omit for the most recent session."),
					"repo":       repo,
					"source":     source,
					"since":      since,
					"range":      strSchema("Python-style slice over the message list. ':100' = first 100, '-50:' = last 50, '305:315' = window. Negative indices count from the end."),
					"outline":    map[string]any{"type": "boolean", "description": "One line per message: [N] role: first-line-truncated. Use to navigate a large session before slicing in with 'range'."},
					"role":       strSchema("Comma-separated roles to keep: 'user', 'assistant', 'tool'. Tool-related roles (toolResult, toolCall, function_call, etc.) collapse to 'tool'. Use 'user,assistant' to skip tool noise in long agent loops."),
				},
			},
		},
		{
			"name":        "recall_sessions",
			"description": "List recent past AI sessions (titles + ids, no bodies). Filter by repo/source/since, or by tags applied with recall_tag.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":   repo,
					"source": source,
					"since":  since,
					"limit":  limit,
					"tags":   tagsFilter,
				},
			},
		},
		{
			"name":        "recall_related",
			"description": "Given a session id, find other past sessions covering the same topic.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": strSchema("Session id to find topically-similar sessions for."),
					"limit":      limit,
				},
				"required": []string{"session_id"},
			},
		},
		{
			"name":        "recall_tag",
			"description": "Attach durable tags to a past session so you can find it again later. Tags survive index rebuilds. Get the session_id from recall_search/recall_sessions, then filter by these tags via the 'tags' arg on recall_search/recall_sessions. Use for memory: tag sessions worth remembering (e.g. 'deploy-rca', 'auth-design').",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": strSchema("Session id to tag (from recall_search/recall_sessions)."),
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "One or more tags to attach. Normalised to lower-case dashed form ('Deploy RCA' → 'deploy-rca').",
					},
				},
				"required": []string{"session_id", "tags"},
			},
		},
		{
			"name":        "recall_untag",
			"description": "Remove tags from a session.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": strSchema("Session id to untag."),
					"tags": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Tags to remove.",
					},
				},
				"required": []string{"session_id", "tags"},
			},
		},
		{
			"name":        "recall_tags",
			"description": "List tags. With no args: every tag and how many sessions carry it. With session_id: the tags on that one session.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": strSchema("Optional: list tags for just this session."),
				},
			},
		},
	}
}

func mcpText(text string, isErr bool) any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}
