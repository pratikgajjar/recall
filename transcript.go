package main

import (
	"fmt"
	"io"
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
}

// parseRange interprets a Python-style slice string against a population of
// `total` items, returning a [lo, hi) half-open interval clamped to bounds.
// "" or ":" return the full range. Negative indices count from the end.
func parseRange(s string, total int) (lo, hi int, err error) {
	s = strings.TrimSpace(s)
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
// carries its own `## msg N/TOTAL role` header so any slice is self-locating —
// the LLM (or human) can re-paginate without re-reading what came before.
func renderTranscript(w io.Writer, s *Session, msgs []Message, opts transcriptOpts) error {
	if opts.Outline {
		return renderOutline(w, s, msgs)
	}
	total := len(msgs)
	lo, hi, err := parseRange(opts.Range, total)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "# %s\n", s.Title)
	if lo == 0 && hi == total {
		fmt.Fprintf(w, "source=%s  project=%s  started=%s  msgs=%d\n\n",
			s.Source, shortProject(s.Project),
			time.UnixMilli(s.StartedAt).Format(time.RFC3339), total)
	} else {
		fmt.Fprintf(w, "source=%s  project=%s  started=%s  msgs=%d  shown=[%d:%d]\n\n",
			s.Source, shortProject(s.Project),
			time.UnixMilli(s.StartedAt).Format(time.RFC3339), total, lo, hi)
	}
	for i := lo; i < hi; i++ {
		m := msgs[i]
		body := strings.TrimSpace(m.Text)
		if body == "" {
			fmt.Fprintf(w, "## msg %d/%d %s\n_(empty)_\n\n", i, total, m.Role)
			continue
		}
		fmt.Fprintf(w, "## msg %d/%d %s\n%s\n\n", i, total, m.Role, body)
	}
	if hi < total {
		fmt.Fprintf(w, "_… %d more messages. Continue with --range %d:_\n", total-hi, hi)
	}
	return nil
}

// renderOutline emits one line per message for cheap navigation. The position
// numbering matches what --range slices, so an LLM can outline → pick → slice.
func renderOutline(w io.Writer, s *Session, msgs []Message) error {
	fmt.Fprintf(w, "# %s\n", s.Title)
	fmt.Fprintf(w, "source=%s  project=%s  msgs=%d  (outline)\n\n",
		s.Source, shortProject(s.Project), len(msgs))
	for i, m := range msgs {
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
		fmt.Fprintf(w, "[%d] %s: %s\n", i, m.Role, first)
	}
	return nil
}
