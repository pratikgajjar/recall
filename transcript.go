package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// transcriptOpts controls how a session is rendered. Both CLI (recall show)
// and the MCP recall_transcript tool route through renderTranscript with these.
type transcriptOpts struct {
	// Range is a Python-style slice string ("FROM:TO") over the message list.
	//   ""        → full transcript
	//   ":100"    → first 100
	//   "-50:"    → last 50
	//   "305:315" → window
	// Negative indices count from the end. Empty bounds default to start/end.
	Range string
	// Outline replaces the full body with one line per message:
	//   [N] role: first-line-truncated
	// Use it as a cheap "ls" over a giant session before slicing in.
	Outline bool
	// Roles is a comma-separated allowlist of canonical roles:
	//   user | assistant | tool   (anything matching "tool"/"function_call"
	//   collapses to "tool" — covers tool, toolResult, toolCall, etc.)
	// Empty means "keep all". Filter applies AFTER range so absolute message
	// indices stay consistent with what recall_search returns.
	Roles string
	// ToolChars caps how much of a tool result's body is rendered. Tool output
	// is the bulk of an agent transcript (measured: 59% of all characters) and
	// is usually a file or command output the reader can re-fetch at will, so
	// the default shows the head and says what was elided. 0 = uncapped.
	ToolChars int
	// MaxChars bounds the rendered body, paging the remainder behind the same
	// next: fragment the pager already emits. 0 = unbounded.
	MaxChars int
	// OmitHeader suppresses the per-render "# title / source=..." block. Set by
	// callers that already printed the session identity, so expanding several
	// hits from one session does not restate it each time.
	OmitHeader bool
}

// defaultMaxChars bounds one transcript read. Set well above a normal window
// (a 20-message slice lands far below it) so only the half-a-session requests
// get paged.
const defaultMaxChars = 20000

// defaultToolChars is the cap applied when a caller expresses no preference.
// Chosen to comfortably hold a short command's full output while cutting the
// file dumps: p50 of tool results in the measured corpus is well under this,
// so typical results pass through whole and only the outliers are clipped.
const defaultToolChars = 600

// clipToolBody trims an over-long tool result, replacing the tail with a count
// of what was dropped. The marker is explicit so a reader knows to go and get
// the rest (recall show --tool-chars 0, or just re-run the command) rather than
// silently trusting a truncated payload.
func clipToolBody(body string, limit int) string {
	if limit <= 0 || len(body) <= limit {
		return body
	}
	cut := body[:limit]
	// Prefer a line boundary so we never end mid-token.
	if nl := strings.LastIndexByte(cut, '\n'); nl > limit/2 {
		cut = cut[:nl]
	}
	dropped := len(body) - len(cut)
	lines := strings.Count(body[len(cut):], "\n")
	return fmt.Sprintf("%s\n… [%d chars, %d lines elided — --tool-chars 0 for all]",
		cut, dropped, lines)
}

// canonicalRole maps adapter-specific role labels into three buckets so a
// filter like --role tool catches tool, toolResult, toolCall, function_call,
// function_call_output, etc. uniformly across sources.
// toolCallArgKeys are the fields that identify what a call actually did, in
// preference order. Shared by every adapter so a transcript reads the same
// whichever tool wrote it. A transcript that shows a tool's output but not its input
// forces the reader to guess: 72% of calls are bash or read, where the command
// and the path are the whole point. Everything else (edit/write bodies, which
// run to thousands of characters) is deliberately not surfaced — those results
// already name their target.
var toolCallArgKeys = []string{"command", "path", "query", "pattern", "file_path"}

// argSummary picks the identifying argument and clips it hard. This is a
// summary for a human or model skimming history, not a reproduction.
func argSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if JSONUnmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range toolCallArgKeys {
		v, ok := m[k].(string)
		if !ok || v == "" {
			continue
		}
		v = strings.TrimSpace(strings.ReplaceAll(v, "\n", " "))
		if len(v) > toolArgMax {
			v = v[:toolArgMax] + "\u2026"
		}
		return v
	}
	return ""
}

// toolArgMax keeps the addition small enough to be worth its tokens; a command
// long enough to be cut is usually identifiable from its first clause.
const toolArgMax = 70

// roleLabel renders a message's role for a transcript header. Adapters may
// qualify a tool role with the tool that produced it ("toolResult:bash"), which
// is worth more than the bare word: without it a reader has to count tool calls
// in the preceding assistant turn and match them to results by position.
//
// The N/TOTAL that used to sit here was dropped: TOTAL is already in the
// session header one line above, and repeating it cost ~20k characters across a
// measured workload for no information.
func roleLabel(role string) string {
	base, tool, hasTool := strings.Cut(role, ":")
	label := base
	if canonicalRole(base) == "tool" {
		label = "tool"
	}
	if hasTool && tool != "" {
		return label + " " + tool
	}
	return label
}

func canonicalRole(r string) string {
	rl := strings.ToLower(r)
	switch rl {
	case "user", "assistant":
		return rl
	}
	if strings.Contains(rl, "tool") || strings.Contains(rl, "function_call") || strings.Contains(rl, "function_result") {
		return "tool"
	}
	return rl
}

func parseRoleFilter(s string) map[string]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(strings.ToLower(r))
		if r != "" {
			out[r] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func keepRole(role string, filter map[string]bool) bool {
	if filter == nil {
		return true
	}
	return filter[canonicalRole(role)]
}

// parseRange interprets a Python-style slice string against a population of
// `total` items, returning a [lo, hi) half-open interval clamped to bounds.
// "" or ":" return the full range. Negative indices count from the end.
func parseRange(s string, total int) (lo, hi int, err error) {
	s = strings.TrimSpace(s)
	// Models routinely emit the value already quoted (range: "\":30\"") or with a
	// stray bracket. Rejecting that costs a whole round-trip — an error the caller
	// reads, plus a corrected retry — to communicate nothing. Strip and carry on.
	s = strings.Trim(s, "\"'`[]() ")
	if s == "" || s == ":" {
		return 0, total, nil
	}
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("range must be FROM:TO (e.g. 100:200, :50, -10:)")
	}
	resolve := func(raw string, def int) (int, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return def, nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("invalid index %q in range", raw)
		}
		if n < 0 {
			n += total
		}
		return n, nil
	}
	if lo, err = resolve(parts[0], 0); err != nil {
		return 0, 0, err
	}
	if hi, err = resolve(parts[1], total); err != nil {
		return 0, 0, err
	}
	if lo < 0 {
		lo = 0
	}
	if hi > total {
		hi = total
	}
	if lo > hi {
		lo = hi
	}
	return lo, hi, nil
}

// renderTranscript writes a session to w according to opts. Every message
// carries its own `## N role` header so any slice is self-locating —
// the LLM (or human) can re-paginate without re-reading what came before.
func renderTranscript(w io.Writer, s *Session, msgs []Message, opts transcriptOpts) error {
	if opts.Outline {
		return renderOutline(w, s, msgs, opts)
	}
	total := len(msgs)
	lo, hi, err := parseRange(opts.Range, total)
	if err != nil {
		return err
	}
	roleFilter := parseRoleFilter(opts.Roles)
	header := fmt.Sprintf("source=%s  project=%s  started=%s  msgs=%d",
		s.Source, shortProject(s.Project),
		time.UnixMilli(s.StartedAt).Format(time.RFC3339), total)
	if !(lo == 0 && hi == total) {
		header += fmt.Sprintf("  shown=[%d:%d]", lo, hi)
	}
	if roleFilter != nil {
		header += "  role=" + strings.ToLower(strings.TrimSpace(opts.Roles))
	}
	if !opts.OmitHeader {
		fmt.Fprintf(w, "# %s\n%s\n\n", s.Title, header)
	}
	// Budget the body. A request like range='0:500' is an agent asking for half a
	// session; answering it literally costs ~200k characters, almost never all of
	// which is wanted. Stop at the budget and hand back the same next: fragment the
	// pager already uses, so nothing is lost — it just arrives a page at a time.
	written := 0
	stopped := -1
	for i := lo; i < hi; i++ {
		m := msgs[i]
		if !keepRole(m.Role, roleFilter) {
			continue
		}
		if opts.MaxChars > 0 && written >= opts.MaxChars && i > lo {
			stopped = i
			break
		}
		body := strings.TrimSpace(m.Text)
		if body == "" {
			n, _ := fmt.Fprintf(w, "## %d %s\n_(empty)_\n\n", i, roleLabel(m.Role))
			written += n
			continue
		}
		if canonicalRole(m.Role) == "tool" {
			body = clipToolBody(body, opts.ToolChars)
		}
		n, _ := fmt.Fprintf(w, "## %d %s\n%s\n\n", i, roleLabel(m.Role), body)
		written += n
	}
	if stopped >= 0 {
		fmt.Fprintf(w, "// stopped at msg %d: %d of the %d requested messages would exceed the"+
			" %s output budget. Continue with range='%d:%d', or narrow with role= / a tighter range.\n",
			stopped, hi-stopped, hi-lo, humanChars(opts.MaxChars), stopped, hi)
		hi = stopped
	}
	// Same next/prev vocabulary as the cross-session pager, but a --range
	// fragment (not a full command) since this renderer is shared by `recall
	// show` and the recall_transcript MCP tool.
	if lo > 0 || hi < total {
		span := hi - lo
		if span <= 0 {
			span = total
		}
		var p []string
		if hi < total {
			end := hi + span
			if end > total {
				end = total
			}
			p = append(p, fmt.Sprintf("next: --range %d:%d", hi, end))
		}
		if lo > 0 {
			start := lo - span
			if start < 0 {
				start = 0
			}
			p = append(p, fmt.Sprintf("prev: --range %d:%d", start, lo))
		}
		fmt.Fprintf(w, "%s  (%d msgs)\n", strings.Join(p, "  "), total)
	}
	return nil
}

// renderOutline emits one line per message for cheap navigation. The position
// numbering matches what --range slices, so an LLM can outline → pick → slice.
// outlineBudget is the size past which an outline stops being cheap navigation
// and becomes the thing you were trying to avoid reading. Beyond it, density
// drops to chapter markers: user turns (the questions that structure a session)
// plus the tool runs between them.
const outlineBudget = 4000

func renderOutline(w io.Writer, s *Session, msgs []Message, opts transcriptOpts) error {
	// Render at full density first; if that blows the budget, fall back to
	// chapters. An explicit role filter says WHICH roles are wanted, not that
	// any amount of them is wanted: asking for role=user,assistant on a 1,991
	// message session returned a 40,000-character "outline", which is the thing
	// an outline exists to avoid reading.
	if opts.Roles != "" {
		var probe strings.Builder
		if err := renderOutlineAt(&probe, s, msgs, opts, false); err != nil {
			return err
		}
		if probe.Len() <= outlineBudget {
			_, err := io.WriteString(w, probe.String())
			return err
		}
		// Chapter density would contradict the filter, so deliver a prefix and
		// say plainly where it stops and how to continue from there.
		return clipOutline(w, probe.String(), len(msgs))
	}
	if opts.Roles == "" {
		var probe strings.Builder
		if err := renderOutlineAt(&probe, s, msgs, opts, false); err != nil {
			return err // includes a malformed range
		}
		if probe.Len() <= outlineBudget {
			_, err := io.WriteString(w, probe.String())
			return err
		}
		fmt.Fprintf(w, "// %d msgs: user turns + tool runs only (full outline is %s)."+
			" Hunting a specific topic? recall_search with session_id costs ~400 chars and"+
			" gives you the msg_idx directly. Else role='user,assistant' for every turn,"+
			" range='FROM:TO' to read a span.\n",
			len(msgs), humanChars(probe.Len()))
		return renderOutlineAt(w, s, msgs, opts, true)
	}
	return renderOutlineAt(w, s, msgs, opts, false)
}

// clipOutline delivers as much of a role-filtered outline as the budget allows
// and names the rest. Cutting silently would leave the caller believing they had
// seen the whole session.
func clipOutline(w io.Writer, full string, total int) error {
	lines := strings.SplitAfter(full, "\n")
	var out strings.Builder
	shown := 0
	for i, ln := range lines {
		if out.Len()+len(ln) > outlineBudget && i > 0 {
			break
		}
		out.WriteString(ln)
		shown = i + 1
	}
	if _, err := io.WriteString(w, out.String()); err != nil {
		return err
	}
	if shown >= len(lines) {
		return nil
	}
	// The first line not delivered names the position to resume from.
	next := 0
	if m := outlinePos.FindStringSubmatch(lines[shown]); m != nil {
		next, _ = strconv.Atoi(m[1])
	}
	_, err := fmt.Fprintf(w, "// outline clipped at msg %d of %d (%s more)."+
		" Continue with outline=true range='%d:', or search with session_id to jump"+
		" straight to a msg_idx.\n",
		next, total, humanChars(len(full)-out.Len()), next)
	return err
}

// outlinePos matches the position marker an outline line starts with, "[12]" or
// "[12-19]", so a clipped outline can say where to resume.
var outlinePos = regexp.MustCompile(`^\[(\d+)`)

// renderOutlineAt emits the outline. When chaptersOnly is set, assistant prose
// is folded into the surrounding tool run instead of getting its own line.
func renderOutlineAt(w io.Writer, s *Session, msgs []Message, opts transcriptOpts, chaptersOnly bool) error {
	total := len(msgs)
	lo, hi, err := parseRange(opts.Range, total)
	if err != nil {
		return err
	}
	roleFilter := parseRoleFilter(opts.Roles)
	header := fmt.Sprintf("source=%s  project=%s  msgs=%d  (outline)",
		s.Source, shortProject(s.Project), len(msgs))
	if !(lo == 0 && hi == total) {
		header += fmt.Sprintf("  shown=[%d:%d]", lo, hi)
	}
	if roleFilter != nil {
		header += "  role=" + strings.ToLower(strings.TrimSpace(opts.Roles))
	}
	fmt.Fprintf(w, "# %s\n%s\n\n", s.Title, header)

	// Consecutive tool messages collapse into a single run line. In an agent
	// session they are the majority of messages, and the first line of a command's
	// output ("No matches found", a shebang, a JSON brace) is close to useless for
	// working out *where* you are. A run keeps what navigation needs — the span to
	// slice on and how much sits inside it — at one line instead of dozens.
	runStart, runCount, runChars := -1, 0, 0
	runTools := map[string]int{}
	var runOrder []string
	flushRun := func() {
		if runCount == 0 {
			return
		}
		end := runStart + runCount - 1
		span := fmt.Sprintf("%d", runStart)
		if end != runStart {
			span = fmt.Sprintf("%d-%d", runStart, end)
		}
		// Naming the tools makes the run line better navigation than the lines it
		// replaces: "bash×4, read×2" locates work that a first-line-of-output never did.
		var names []string
		for _, n := range runOrder {
			if c := runTools[n]; c > 1 {
				names = append(names, fmt.Sprintf("%s×%d", n, c))
			} else {
				names = append(names, n)
			}
		}
		detail := humanChars(runChars)
		if len(names) > 0 {
			detail += ": " + strings.Join(names, ", ")
		}
		fmt.Fprintf(w, "[%s] tool ×%d (%s)\n", span, runCount, detail)
		runStart, runCount, runChars = -1, 0, 0
		runTools = map[string]int{}
		runOrder = nil
	}
	addRun := func(i int, m Message, names []string) {
		if runCount == 0 {
			runStart = i
		}
		runCount++
		runChars += len(m.Text)
		for _, n := range names {
			if runTools[n] == 0 {
				runOrder = append(runOrder, n)
			}
			runTools[n]++
		}
	}

	// Chapter mode alone does not bound a session with hundreds of user turns,
	// so the budget is also enforced as a hard stop — with the range to resume
	// from, since an outline can itself be sliced.
	written := 0
	stopped := -1
	for i := lo; i < hi; i++ {
		m := msgs[i]
		if !keepRole(m.Role, roleFilter) {
			continue
		}
		if chaptersOnly && written >= outlineBudget && i > lo {
			stopped = i
			break
		}
		if canonicalRole(m.Role) == "tool" {
			addRun(i, m, nil)
			continue
		}
		// An assistant turn that is nothing but tool-call markers is tool activity
		// too — "[tool:read]" on its own line is not prose worth a slot.
		if names, only := toolMarkerNames(m.Text); only {
			addRun(i, m, names)
			continue
		}
		// Chapter mode: the user's turns structure the session, so they stay;
		// the agent's narration between them folds into the run.
		if chaptersOnly && canonicalRole(m.Role) == "assistant" {
			addRun(i, m, nil)
			continue
		}
		flushRun()
		first := strings.TrimSpace(m.Text)
		if nl := strings.IndexByte(first, '\n'); nl > 0 {
			first = first[:nl]
		}
		if first == "" {
			first = "(empty)"
		}
		if len(first) > 100 {
			first = first[:100] + "…"
		}
		n, _ := fmt.Fprintf(w, "[%d] %s: %s\n", i, m.Role, first)
		written += n
	}
	flushRun()
	if stopped >= 0 {
		fmt.Fprintf(w, "// outline truncated at msg %d of %d. Continue with range='%d:' outline=true.\n",
			stopped, total, stopped)
	}
	return nil
}

// toolMarkerRe matches the tool-call markers the adapters emit: pi's
// "[tool:name]", claude's "[tool_use:name]", codex's "[call name]".
// The trailing (?:\s.*)? matters: adapters append a clipped argument after the
// marker ("[tool:bash] git log --oneline"), and anchoring on \]$ would stop
// recognising those as tool activity, so they would each take an outline line
// instead of folding into a run.
var toolMarkerRe = regexp.MustCompile(`^\[(?:tool|tool_use):([^\]]+)\](?:\s.*)?$|^\[call ([^\]]+)\]`)

// toolMarkerNames reports the tools named in text, and whether text consists of
// nothing but those markers (i.e. carries no prose of its own).
func toolMarkerNames(text string) ([]string, bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var names []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		m := toolMarkerRe.FindStringSubmatch(ln)
		if m == nil {
			return nil, false
		}
		names = append(names, strings.TrimSpace(m[1]+m[2]))
	}
	return names, len(names) > 0
}

func humanChars(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk chars", float64(n)/1000)
	}
	return fmt.Sprintf("%d chars", n)
}

// contextHitLimit bounds how many hits get expanded. Each expansion re-reads a
// session from its source, so this is a cost ceiling as much as a token one.
const contextHitLimit = 3

// printHitsWithContext prints each hit followed by the messages around it,
// collapsing the two-step "search, then show --range N-5:N+5" into one call.
// Same idea as grep -C: the match is rarely useful without its neighbours.
func printHitsWithContext(ctx context.Context, w io.Writer, ix *Index, hits []Hit, n int) error {
	shown := hits
	if len(shown) > contextHitLimit {
		shown = shown[:contextHitLimit]
	}
	// All from one session (the session-scoped case): identity once, not per hit.
	shared := loneSession(shown) != ""
	if shared {
		h0 := shown[0]
		fmt.Fprintf(w, "## %s  %s  %s\n%s\nid=%s  (%d hits expanded)\n\n",
			h0.StartedTime().Format("2006-01-02 15:04"), h0.Source,
			shortProject(h0.Project), h0.Title, h0.SessionID, len(shown))
	}
	for i, h := range shown {
		if !shared {
			fmt.Fprintf(w, "## %s  %s  %s\n%s\n",
				h.StartedTime().Format("2006-01-02 15:04"), h.Source,
				shortProject(h.Project), h.Title)
			fmt.Fprintf(w, "id=%s  msg=%d\n\n", h.SessionID, h.MsgIdx)
		}

		if h.MsgIdx < 0 { // a title-only match has no message to expand
			fmt.Fprintf(w, "%s\n\n", h.Snippet)
			continue
		}
		lo := h.MsgIdx - n
		if lo < 0 {
			lo = 0
		}
		if err := printTranscriptTo(ctx, w, ix, h.SessionID, false, transcriptOpts{
			Range:      fmt.Sprintf("%d:%d", lo, h.MsgIdx+n+1),
			ToolChars:  defaultToolChars,
			MaxChars:   defaultMaxChars,
			OmitHeader: shared,
		}); err != nil {
			return err
		}
		if i < len(shown)-1 {
			fmt.Fprintln(w)
		}
	}
	if rest := len(hits) - len(shown); rest > 0 {
		fmt.Fprintf(w, "\n// %d more hits not expanded; re-run without --context to list them all.\n", rest)
	}
	return nil
}
