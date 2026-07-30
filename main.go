package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// version is set by goreleaser via -ldflags "-X main.version=..." (a plain
// string var with no initializer, so the linker's -X can patch it).
var version string

// recallVersion returns the release version: the ldflag-injected value, else
// the module version `go install` embeds (e.g. "v0.2.5"), else "dev" for a
// source build. This keeps `go install ...@vX` reporting the right version
// even though it never runs goreleaser's ldflags.
func recallVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}

func usage() {
	fmt.Fprintf(os.Stderr, `recall %s — your AI chat history, searchable across Cursor, Claude Code, Codex, pi.

USAGE
  recall <query> [flags]           search; prints ranked hits (alias: 'find')
  recall last [flags]              full transcript of the most recent matching session
  recall show <session-id>         full transcript of one session (re-read from source)
  recall sessions [flags]          list sessions, no body
  recall related <session-id>      sessions covering the same topic as this one
  recall tag                       list all tags + counts (git-tag style)
  recall tag <session-id> <tag>…   attach durable tags (survive reindex)
  recall tag -d <session-id> <tag>…  remove tags
  recall tag -l [session-id]       list all tags, or one session's tags
  recall open <session-id>         reopen in the source tool (cursor://, claude --resume, …)
  recall stats [flags]             session/message counts by source/project
  recall index [--full]            (re)build the local index from all sources
  recall mcp                       run an MCP server (Claude Code, Codex, Cursor, …)
  recall doctor                    health check + source detection
  recall version

FLAGS
  --repo PATH                      restrict to a specific project folder
  --since DURATION                 e.g. 24h, 7d
  --limit N                        default 30
  --tag SELECTOR                   filter, repeatable (AND). A user tag, or a
                                   reserved facet: source:cursor|claude|codex|pi
  --json                           machine-readable output (snake_case)

EXAMPLES
  recall "remote chat"                                # all sources, all repos
  recall "remote chat" --repo ~/code/acme-api  # one project
  recall "remote chat" --tag source:pi --since 30d    # one tool, recent
  recall last --repo .                                # most recent here, full
  recall related cursor:bc9f2a9b-…                    # neighbour topics
  recall stats --since 7d                             # what did I work on this week?
  recall tag pi:019ed75b-… deploy-rca                 # remember this session
  recall sessions --tag deploy-rca                    # find tagged sessions
  recall sessions --tag source:cursor --tag deploy-rca  # facet + tag, AND

DATA
  index    ~/.recall/index.sqlite   (disposable — rebuild any time; tags persist)
  sources  Cursor SQLite KV, Claude/Codex/pi JSONL files
           (read-only — recall never mutates source data)
`, recallVersion())
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "-h", "--help", "help":
		usage()
	case "version", "--version", "-v":
		fmt.Println(recallVersion())
	case "index":
		if err := runIndex(args); err != nil {
			fatal(err)
		}
	case "doctor":
		if err := runDoctor(args); err != nil {
			fatal(err)
		}
	case "find":
		if err := runFind(args); err != nil {
			fatal(err)
		}
	case "last":
		if err := runLast(args); err != nil {
			fatal(err)
		}
	case "show":
		if err := runShow(args); err != nil {
			fatal(err)
		}
	case "sessions":
		if err := runSessions(args); err != nil {
			fatal(err)
		}
	case "open":
		if err := runOpen(args); err != nil {
			fatal(err)
		}
	case "stats":
		if err := runStats(args); err != nil {
			fatal(err)
		}
	case "related":
		if err := runRelated(args); err != nil {
			fatal(err)
		}
	case "mcp":
		if err := runMCP(args); err != nil {
			fatal(err)
		}
	case "tag":
		if err := runTag(args); err != nil {
			fatal(err)
		}
	case "plugin":
		if err := runPlugin(args); err != nil {
			fatal(err)
		}
	default:

		if err := runFind(os.Args[1:]); err != nil {
			fatal(err)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "recall: "+err.Error())
	os.Exit(1)
}

// defaultIndexPath is ~/.recall/index.sqlite, overridable with RECALL_INDEX.
//
// The override exists so schema changes can be tried against a scratch index
// without migrating the real one out from under other running recall builds:
//
//	RECALL_INDEX=/tmp/scratch.sqlite ./recall index --full
func defaultIndexPath() string {
	if p := os.Getenv("RECALL_INDEX"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".recall", "index.sqlite")
}

func defaultAdapters() []Adapter {
	home, _ := os.UserHomeDir()
	ads := []Adapter{
		&CursorAdapter{UserDir: filepath.Join(home, "Library", "Application Support", "Cursor", "User")},
		&ClaudeAdapter{Root: filepath.Join(home, ".claude", "projects")},
		&CodexAdapter{Root: filepath.Join(home, ".codex", "sessions")},
		&PiAdapter{Root: filepath.Join(home, ".pi", "agent", "sessions")},
		&CursorAgentAdapter{Root: filepath.Join(home, ".cursor", "projects")},
	}
	// User-supplied Lua plugins extend the set without recompiling: each defines
	// in Lua what records to extract; Go indexes them like any other source. A
	// plugin may also override a built-in of the same id (drop a claude.lua to
	// replace the Go Claude adapter).
	plugins := discoverLuaAdapters(filepath.Join(home, ".recall", "plugins"))
	return mergeAdapters(ads, plugins)
}

// mergeAdapters overlays Lua plugins onto the built-ins: a plugin whose id
// matches a built-in replaces it; new ids are appended. This both enables
// "redefine a source in Lua" and avoids two adapters sharing one id.
func mergeAdapters(builtins, plugins []Adapter) []Adapter {
	out := append([]Adapter(nil), builtins...)
	for _, p := range plugins {
		replaced := false
		for i, a := range out {
			if a.ID() == p.ID() {
				out[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, p)
		}
	}
	return out
}

// stringSlice is a repeatable string flag: `--tag a --tag b` collects [a, b].
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type commonFlags struct {
	repo   string
	in     string
	since  string
	after  string
	before string
	limit  int
	json   bool
	tags   stringSlice
}

func (cf *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&cf.repo, "repo", "", "restrict to a project folder")
	fs.StringVar(&cf.in, "in", "", "restrict to one session id (search inside a transcript);\n\t\".\" = the newest session for this repo")
	fs.StringVar(&cf.since, "since", "", "alias for --after (lower time bound)")
	fs.StringVar(&cf.after, "after", "", "only sessions started after this: 7d, an epoch-ms, or YYYY-MM-DD")
	fs.StringVar(&cf.before, "before", "", "only sessions started before this (for paging back through history)")
	fs.IntVar(&cf.limit, "limit", 30, "max results")
	fs.BoolVar(&cf.json, "json", false, "machine-readable output")
	fs.Var(&cf.tags, "tag", "filter selector, repeatable (AND): a user tag, or a\n\treserved facet like source:cursor")
}

// reservedFacets maps a selector prefix (before ':') to the sessions column it
// filters. These are DERIVED facets — read-only, regenerated each ingest — so
// they can be queried through --tag but never authored as a tag.
var reservedFacets = map[string]string{"source": "source", "src": "source"}

// parseSelectors splits --tag tokens into user tags (session_tags rows) and
// reserved facet values (sessions columns). A token "key:value" whose key is
// reserved routes to the facet; everything else is a user tag — so a user tag
// may still contain ':' as long as its prefix isn't a reserved facet.
func parseSelectors(tokens []string) (tags []string, source string) {
	for _, t := range tokens {
		if k, v, ok := strings.Cut(t, ":"); ok {
			if facet := reservedFacets[strings.ToLower(strings.TrimSpace(k))]; facet != "" {
				if facet == "source" {
					source = strings.ToLower(strings.TrimSpace(v))
				}
				continue
			}
		}
		tags = append(tags, t)
	}
	return
}

// reservedFacetError rejects authoring a reserved facet as a tag. Returns "" if
// the token is a legal user tag.
func reservedFacetError(token string) string {
	if k, _, ok := strings.Cut(token, ":"); ok {
		if facet := reservedFacets[strings.ToLower(strings.TrimSpace(k))]; facet != "" {
			return fmt.Sprintf("%q is a reserved facet (derived from the session, not a tag); filter with `--tag %s` instead", token, token)
		}
	}
	return ""
}

func splitFlagsAndArgs(in []string, boolFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(in); i++ {
		a := in[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)

		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if boolFlags[name] {
			continue
		}
		if i+1 < len(in) {
			flags = append(flags, in[i+1])
			i++
		}
	}
	return
}

// resolveRepo normalizes a --repo value: expands ~, makes it absolute, and
// walks up to the enclosing git root so `--repo .` from a subdirectory means
// the whole repo. Returns "" unchanged.
func resolveRepo(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	for d := p; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return p
		}
		d = parent
	}
}

var sharedBoolFlags = map[string]bool{"json": true, "outline": true}

func (cf *commonFlags) toSearchOpts() (SearchOpts, error) {
	tags, source := parseSelectors(cf.tags)
	opts := SearchOpts{
		Source:    source,
		Project:   resolveRepo(cf.repo),
		SessionID: cf.in,
		Limit:     cf.limit,
		Tags:      tags,
	}
	// --since is a permanent alias for --after (lower time bound). --after wins
	// if both are given.
	lower := cf.after
	if lower == "" {
		lower = cf.since
	}
	if lower != "" {
		t, err := parseInstant(lower)
		if err != nil {
			return opts, err
		}
		opts.After = t
	}
	if cf.before != "" {
		t, err := parseInstant(cf.before)
		if err != nil {
			return opts, err
		}
		opts.Before = t
	}
	return opts, nil
}

// parseInstant resolves a time bound to an absolute epoch-ms. It accepts:
//   - a duration ("7d", "24h", "90m") — that long before now;
//   - a raw epoch-ms integer — e.g. a started_at_ms copied from --json output,
//     which makes keyset paging (`--before <oldest_started_at_ms>`) trivial;
//   - an RFC3339 timestamp or a YYYY-MM-DD date.
func parseInstant(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if isAllDigits(s) {
		return strconv.ParseInt(s, 10, 64)
	}
	if d, err := parseDuration(s); err == nil {
		return time.Now().Add(-d).UnixMilli(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("invalid time %q (use a duration like 7d, an epoch-ms, or YYYY-MM-DD)", s)
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// parseDuration extends time.ParseDuration with a "d" (day) suffix.
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h")
		if err != nil {
			return 0, err
		}
		return days * 24, nil
	}
	return time.ParseDuration(s)
}

func openIndexOrFail() (*Index, error) {
	path := defaultIndexPath()
	if _, err := os.Stat(path); err != nil {
		return nil, errors.New("no index yet — run `recall index` first")
	}
	return openIndexRead(path)
}

// openIndexWriteOrFail opens the index for writing (tags). Unlike search, which
// uses a read-only handle, tagging mutates session_tags.
func openIndexWriteOrFail() (*Index, error) {
	path := defaultIndexPath()
	if _, err := os.Stat(path); err != nil {
		return nil, errors.New("no index yet — run `recall index` first")
	}
	return openIndex(path)
}

// runTag: recall tag <session-id> <tag>...
// runTag follows the `git tag` model — one verb, mode by flag:
//
//	recall tag                      list all tags + counts (like bare `git tag`)
//	recall tag -l [session-id]      list all, or one session's tags
//	recall tag <session-id> <tag>…  attach tags
//	recall tag -d <session-id> <tag>…  remove tags
func runTag(args []string) error {
	flagArgs, pos := splitFlagsAndArgs(args, map[string]bool{"d": true, "l": true, "json": true})
	fs := flag.NewFlagSet("tag", flag.ExitOnError)
	del := fs.Bool("d", false, "delete the given tags from the session")
	list := fs.Bool("l", false, "list tags (all, or for one session)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	// List mode: explicit -l, or no positionals at all (bare `recall tag`), or a
	// lone session id with no tags to add.
	if *list || len(pos) == 0 || (!*del && len(pos) == 1) {
		return listTags(pos, *asJSON)
	}

	if len(pos) < 2 {
		return errors.New("usage: recall tag <session-id> <tag>...  (or -d to remove, -l to list)")
	}
	id, tags := pos[0], pos[1:]
	for _, t := range tags {
		if msg := reservedFacetError(t); msg != "" {
			return errors.New(msg)
		}
	}
	ix, err := openIndexWriteOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()

	if *del {
		removed, err := ix.RemoveTags(id, tags)
		if err != nil {
			return err
		}
		cur, err := ix.SessionTags(id)
		if err != nil {
			return err
		}
		fmt.Printf("untagged %s (-%d) → %s\n", id, removed, strings.Join(cur, " "))
		return nil
	}

	added, err := ix.AddTags(id, tags)
	if err != nil {
		return err
	}
	cur, err := ix.SessionTags(id)
	if err != nil {
		return err
	}
	fmt.Printf("tagged %s (+%d) → %s\n", id, added, strings.Join(cur, " "))
	return nil
}

// listTags backs `recall tag -l`: all tags with counts, or one session's tags.
func listTags(pos []string, asJSON bool) error {
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()

	if len(pos) > 0 {
		tags, err := ix.SessionTags(pos[0])
		if err != nil {
			return err
		}
		if asJSON {
			return JSONNewEncoder(os.Stdout).Encode(tags)
		}
		if len(tags) == 0 {
			fmt.Println("no tags")
			return nil
		}
		fmt.Println(strings.Join(tags, " "))
		return nil
	}

	all, err := ix.AllTags()
	if err != nil {
		return err
	}
	if asJSON {
		return JSONNewEncoder(os.Stdout).Encode(all)
	}
	if len(all) == 0 {
		fmt.Println("no tags yet — tag a session with `recall tag <session-id> <tag>`")
		return nil
	}
	for _, tc := range all {
		fmt.Printf("%4d  %s\n", tc.Count, tc.Tag)
	}
	return nil
}

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	full := fs.Bool("full", false, "ignore checkpoints and reindex everything")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// First Ctrl+C cancels ctx (graceful). A second one force-exits, in case a
	// syscall is briefly uninterruptible.
	go func() {
		<-ctx.Done()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		fmt.Fprintln(os.Stderr, "\nforce quit")
		os.Exit(130)
	}()

	ix, err := openIndex(defaultIndexPath())
	if err != nil {
		return err
	}
	defer ix.Close()

	ix.BulkMode(true, *full)

	start := time.Now()
	var grand int
	for _, ad := range defaultAdapters() {
		if ctx.Err() != nil {
			break
		}
		if !ad.Available() {
			fmt.Printf("  %-7s skipped (not installed)\n", ad.ID())
			continue
		}
		t0 := time.Now()
		var prev string
		if !*full {
			prev = ix.GetMeta("ckpt:" + ad.ID())
		}
		fmt.Printf("  %-7s scanning…", ad.ID())
		os.Stdout.Sync()

		// emit ingests a batch and commits the checkpoint that's valid as of
		// that batch, so progress is durable and resumable mid-provider.
		var nSess, nMsg int
		emit := func(sessions []Session, msgs []Message, ckpt string) error {
			if err := ix.IngestBatch(ctx, ad.ID(), sessions, msgs); err != nil {
				return err
			}
			if ckpt != "" {
				_ = ix.SetMeta("ckpt:"+ad.ID(), ckpt)
			}
			nSess += len(sessions)
			nMsg += len(msgs)
			fmt.Printf("\r\033[K  %-7s indexing… %d sessions", ad.ID(), nSess)
			os.Stdout.Sync()
			return nil
		}

		var scanErr error
		if st, ok := ad.(StreamingAdapter); ok {
			scanErr = st.ScanStream(ctx, prev, emit)
		} else {
			sessions, msgs, next, err := ad.Scan(ctx, prev)
			if err != nil {
				scanErr = err
			} else {
				scanErr = emit(sessions, msgs, next)
			}
		}
		if scanErr != nil {
			if ctx.Err() != nil {
				fmt.Printf("\r\033[K")
				break
			}
			fmt.Printf("\r\033[K  %-7s error: %v\n", ad.ID(), scanErr)
			continue
		}
		grand += nSess
		fmt.Printf("\r\033[K  %-7s %6d sessions  %7d messages  %s\n",
			ad.ID(), nSess, nMsg, time.Since(t0).Round(time.Millisecond))
	}
	if ctx.Err() != nil {
		ix.BulkMode(false, false)
		fmt.Printf("interrupted — indexed %d sessions in %s (checkpoints saved, safe to resume)\n",
			grand, time.Since(start).Round(time.Millisecond))
		return nil
	}

	if *full {
		fmt.Printf("  optimizing FTS index…")
		os.Stdout.Sync()
	}
	ix.BulkMode(false, *full)
	if *full {
		fmt.Printf("\r\033[K")
	}
	_ = ix.SetMeta("indexed_at", time.Now().Format(time.RFC3339))
	fmt.Printf("indexed %d sessions in %s → %s\n",
		grand, time.Since(start).Round(time.Millisecond), defaultIndexPath())
	return nil
}

func runDoctor(_ []string) error {
	fmt.Printf("recall %s\n\n", recallVersion())
	fmt.Println("sources:")
	for _, ad := range defaultAdapters() {
		ok := ad.Available()
		mark := "✓"
		if !ok {
			mark = "✗"
		}
		fmt.Printf("  %s %-7s %s\n", mark, ad.ID(), adapterPath(ad))
	}
	fmt.Println()

	idxPath := defaultIndexPath()
	if _, err := os.Stat(idxPath); err != nil {
		fmt.Printf("index: not built yet — run `recall index`\n")
		return nil
	}
	st, _ := os.Stat(idxPath)
	ix, err := openIndexRead(idxPath)
	if err != nil {
		return err
	}
	defer ix.Close()
	counts, err := ix.Counts()
	if err != nil {
		return err
	}
	fmt.Printf("index: %s (%s)\n", idxPath, humanSize(st.Size()))
	total := 0

	for _, ad := range defaultAdapters() {
		if c, ok := counts[ad.ID()]; ok {
			fmt.Printf("  %-7s %d sessions\n", ad.ID(), c)
			total += c
		}
	}

	for src, c := range counts {
		known := false
		for _, ad := range defaultAdapters() {
			if ad.ID() == src {
				known = true
				break
			}
		}
		if !known {
			fmt.Printf("  %-7s %d sessions (orphan)\n", src, c)
			total += c
		}
	}
	fmt.Printf("  total   %d sessions\n", total)
	return nil
}

func adapterPath(a Adapter) string {
	switch v := a.(type) {
	case *CursorAdapter:
		return filepath.Join(v.UserDir, "globalStorage/state.vscdb")
	case *ClaudeAdapter:
		return v.Root
	case *CodexAdapter:
		return v.Root
	case *PiAdapter:
		return v.Root
	case *luaAdapter:
		return v.path
	}
	return ""
}

func humanSize(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for v := n / u; v >= u; v /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// resolveSessionDot turns SessionID "." into the newest indexed session for
// the current repo (falling back to newest anywhere), so `recall find --in .`
// means "search inside the session I am in right now" — the on-disk transcript
// survives context compaction, so this recovers what the agent forgot.
func resolveSessionDot(ix *Index, opts *SearchOpts) error {
	if opts.SessionID != "." {
		return nil
	}
	probe := SearchOpts{Limit: 1}
	if cwd, err := os.Getwd(); err == nil {
		probe.Project = resolveRepo(cwd)
	}
	hits, err := ix.Search("", probe)
	if err == nil && len(hits) == 0 && probe.Project != "" {
		hits, err = ix.Search("", SearchOpts{Limit: 1})
	}
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return fmt.Errorf("no sessions indexed yet")
	}
	opts.SessionID = hits[0].SessionID
	return nil
}

func runFind(args []string) error {
	fs := flag.NewFlagSet("find", flag.ExitOnError)
	var cf commonFlags
	cf.register(fs)
	flagArgs, posArgs := splitFlagsAndArgs(args, sharedBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	query := strings.Join(posArgs, " ")
	opts, err := cf.toSearchOpts()
	if err != nil {
		return err
	}
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	if err := resolveSessionDot(ix, &opts); err != nil {
		return err
	}
	defer ix.Close()
	hits, err := ix.Search(query, opts)
	if err != nil {
		return err
	}
	if cf.json {
		return JSONNewEncoder(os.Stdout).Encode(hits)
	}
	printHits(os.Stdout, hits)
	printPager(os.Stdout, "find", query, cf, hits)
	return nil
}

// printPager emits one-line, copy-pasteable next/prev page commands so an agent
// can traverse all matches mechanically. "next" walks back through history
// (--before the oldest hit); "prev" returns toward the present (--after the
// newest). Epoch-ms is used because parseInstant accepts it. Kept terse on
// purpose — this rides along in every result.
func printPager(w io.Writer, verb, query string, cf commonFlags, hits []Hit) {
	if len(hits) == 0 {
		return
	}
	var tmin, tmax int64
	for _, h := range hits {
		if h.StartedAt == 0 {
			continue
		}
		if tmin == 0 || h.StartedAt < tmin {
			tmin = h.StartedAt
		}
		if h.StartedAt > tmax {
			tmax = h.StartedAt
		}
	}
	base := pagerBase(verb, query, cf)
	// Always offer the older page when there's a time cursor: per-session dedup
	// makes len(hits) an unreliable "is there more" signal, and keyset paging
	// self-terminates (an empty older page prints no footer of its own).
	if tmin > 0 {
		fmt.Fprintf(w, "next: %s --before %d\n", base, tmin)
	}
	if (cf.before != "" || cf.after != "" || cf.since != "") && tmax > 0 {
		fmt.Fprintf(w, "prev: %s --after %d\n", base, tmax+1)
	}
}

// pagerBase rebuilds the invocation minus any time bound (older/newer append
// their own --before/--after).
func pagerBase(verb, query string, cf commonFlags) string {
	parts := []string{"recall", verb}
	if query != "" {
		parts = append(parts, strconv.Quote(query))
	}
	if cf.repo != "" {
		parts = append(parts, "--repo", cf.repo)
	}
	if cf.in != "" {
		parts = append(parts, "--in", cf.in)
	}
	for _, t := range cf.tags {
		parts = append(parts, "--tag", t)
	}
	if cf.limit > 0 {
		parts = append(parts, "--limit", strconv.Itoa(cf.limit))
	}
	return strings.Join(parts, " ")
}

func printHits(w io.Writer, hits []Hit) {
	if len(hits) == 0 {
		fmt.Fprintln(w, "no matches")
		return
	}
	for _, h := range hits {
		when := h.StartedTime().Format("2006-01-02 15:04")
		proj := shortProject(h.Project)
		title := h.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "%s  %-6s  %-24s  %s\n", when, h.Source, truncate(proj, 24), truncate(title, 60))
		fmt.Fprintf(w, "    id=%s  msg=%d  role=%s\n", h.SessionID, h.MsgIdx, h.Role)
		if h.Snippet != "" {
			fmt.Fprintf(w, "    %s\n", strings.ReplaceAll(h.Snippet, "\n", " "))
		}
		fmt.Fprintln(w)
	}
}

func shortProject(p string) string {
	if p == "" {
		return "(none)"
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func runLast(args []string) error {
	fs := flag.NewFlagSet("last", flag.ExitOnError)
	var cf commonFlags
	cf.register(fs)
	rng := fs.String("range", "", "Python-style slice FROM:TO")
	outline := fs.Bool("outline", false, "one line per message")
	roles := fs.String("role", "", "comma-separated roles to keep: user,assistant,tool")
	flagArgs, _ := splitFlagsAndArgs(args, sharedBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	opts, _ := cf.toSearchOpts()
	opts.Limit = 1
	if opts.Project == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.Project = resolveRepo(cwd)
		}
	}
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()
	hits, err := ix.Search("", opts)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return fmt.Errorf("no sessions found for %s", opts.Project)
	}
	return printTranscript(context.Background(), ix, hits[0].SessionID, cf.json, transcriptOpts{Range: *rng, Outline: *outline, Roles: *roles})
}

func runShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable")
	rng := fs.String("range", "", "Python-style slice FROM:TO (e.g. 100:200, :50, -10:)")
	outline := fs.Bool("outline", false, "one line per message: [N] role: first-line")
	roles := fs.String("role", "", "comma-separated roles to keep: user,assistant,tool")
	flagArgs, posArgs := splitFlagsAndArgs(args, sharedBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(posArgs) < 1 {
		return errors.New("usage: recall show <session-id> [--range FROM:TO | --outline | --role user,assistant]")
	}
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()
	return printTranscript(context.Background(), ix, posArgs[0], *asJSON, transcriptOpts{Range: *rng, Outline: *outline, Roles: *roles})
}

func printTranscript(ctx context.Context, ix *Index, id string, asJSON bool, opts transcriptOpts) error {
	s, err := ix.LookupSession(id)
	if err != nil {
		return fmt.Errorf("session %s: %w", id, err)
	}
	ad := adapterFor(s.Source)
	if ad == nil {
		return fmt.Errorf("no adapter for source %q", s.Source)
	}
	msgs, err := ad.Fetch(ctx, s.SourceID)
	if err != nil {
		return err
	}
	if asJSON {
		return JSONNewEncoder(os.Stdout).Encode(map[string]any{
			"session":  s,
			"messages": msgs,
		})
	}
	return renderTranscript(os.Stdout, s, msgs, opts)
}

func adapterFor(source string) Adapter {
	for _, a := range defaultAdapters() {
		if a.ID() == source {
			return a
		}
	}
	return nil
}

func runOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	dryRun := fs.Bool("print", false, "print the open URI/cmd instead of launching")
	flagArgs, posArgs := splitFlagsAndArgs(args, map[string]bool{"print": true})
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(posArgs) == 0 {
		return errors.New("usage: recall open <session-id>")
	}
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()
	s, err := ix.LookupSession(posArgs[0])
	if err != nil {
		return fmt.Errorf("session %s: %w", posArgs[0], err)
	}
	ad := adapterFor(s.Source)
	if ad == nil {
		return fmt.Errorf("no adapter for source %q", s.Source)
	}
	target := ad.OpenURL(s.SourceID)
	if target == "" {
		return fmt.Errorf("source %q does not support deep-link open", s.Source)
	}
	if *dryRun {
		fmt.Println(target)
		return nil
	}
	switch {
	case strings.HasPrefix(target, "cursor://"):

		return runShell("open", target)
	default:

		fmt.Println(target)
		return nil
	}
}

func runShell(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	var cf commonFlags
	cf.register(fs)
	topN := fs.Int("projects", 8, "projects to list per source (0 = all)")
	topM := fs.Int("models", 12, "models to list (0 = all)")
	csvPrefix := fs.String("csv", "", "export to <prefix>projects.csv and <prefix>models.csv\n\tinstead of printing tables (raw unformatted numbers)")
	flagArgs, _ := splitFlagsAndArgs(args, sharedBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	opts, err := cf.toSearchOpts()
	if err != nil {
		return err
	}
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()
	rows, err := ix.Stats(opts)
	if err != nil {
		return err
	}
	models, err := ix.ModelStats(opts)
	if err != nil {
		return err
	}
	if cf.json {
		return JSONNewEncoder(os.Stdout).Encode(struct {
			Projects []StatRow  `json:"projects"`
			Models   []ModelRow `json:"models"`
		}{rows, models})
	}
	if *csvPrefix != "" {
		return exportStatsCSV(*csvPrefix, rows, models)
	}

	var totalS, totalM int
	var totalTok int64
	var totalCost float64
	bySource := map[string][]StatRow{}
	order := []string{}
	for _, r := range rows {
		if _, ok := bySource[r.Source]; !ok {
			order = append(order, r.Source)
		}
		bySource[r.Source] = append(bySource[r.Source], r)
		totalS += r.Sessions
		totalM += r.Messages
		totalTok += r.Tokens
		totalCost += r.CostUSD
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SOURCE\tSESSIONS\tMESSAGES\tTOKENS\tCOST\tPROJECT")
	for _, src := range order {
		group := bySource[src]
		var ss, ms int
		var tok int64
		var cost float64
		allEst := true
		for _, r := range group {
			ss += r.Sessions
			ms += r.Messages
			tok += r.Tokens
			cost += r.CostUSD
			if !r.Estimated {
				allEst = false
			}
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t(all)\n",
			src, ss, ms, fmtTokens(tok, allEst), fmtCost(cost))

		shown := group
		if *topN > 0 && len(shown) > *topN {
			shown = shown[:*topN]
		}
		for _, r := range shown {
			proj := r.Project
			if proj == "" {
				proj = "(none)"
			}
			fmt.Fprintf(w, "\t%d\t%d\t%s\t%s\t  %s\n",
				r.Sessions, r.Messages, fmtTokens(r.Tokens, r.Estimated),
				fmtCost(r.CostUSD), shortProject(proj))
		}
		if n := len(group) - len(shown); n > 0 {
			fmt.Fprintf(w, "\t\t\t\t\t  … %d more (--projects 0)\n", n)
		}
	}
	fmt.Fprintf(w, "\nTOTAL\t%d\t%d\t%s\t%s\t\n",
		totalS, totalM, fmtTokens(totalTok, false), fmtCost(totalCost))
	if err := w.Flush(); err != nil {
		return err
	}

	if len(models) > 0 {
		shown := models
		if *topM > 0 && len(shown) > *topM {
			shown = shown[:*topM]
		}
		mw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(mw, "\nMODEL\tSOURCE\tSESSIONS\tTOKENS\tCACHED\tCOST")
		for _, m := range shown {
			cached := ""
			if m.Tokens > 0 && m.CacheRead > 0 {
				cached = fmt.Sprintf("%d%%", 100*m.CacheRead/m.Tokens)
			}
			// Cursor names a best-of-N composer after every model it ran, which
			// is one 60-char cell that pads the column for every other row.
			fmt.Fprintf(mw, "%s\t%s\t%d\t%s\t%s\t%s\n",
				truncate(m.Model, 28), m.Source, m.Sessions,
				fmtTokens(m.Tokens, m.Estimated), cached, fmtCost(m.CostUSD))
		}
		if n := len(models) - len(shown); n > 0 {
			fmt.Fprintf(mw, "… %d more\t\t\t\t\t\n", n)
		}
		if err := mw.Flush(); err != nil {
			return err
		}
	}
	if totalCost == 0 {
		fmt.Fprintln(os.Stdout, "\n~ = estimated from text length; cost is only known where the source records it.")
	}
	return nil
}

// exportStatsCSV writes the two stats tables as CSV: <prefix>projects.csv and
// <prefix>models.csv. Numbers go out raw — no 8.2B, no $, no ~ — because the
// point of exporting is to do arithmetic on them elsewhere. The `estimated`
// column carries what the ~ meant, so a spreadsheet can filter guesses out.
// Row caps (--projects/--models) are deliberately ignored here; an export is
// everything.
func exportStatsCSV(prefix string, rows []StatRow, models []ModelRow) error {
	projRecords := [][]string{{"source", "project", "sessions", "messages",
		"tokens", "cache_read", "cost_usd", "estimated"}}
	for _, r := range rows {
		projRecords = append(projRecords, []string{
			r.Source, r.Project,
			strconv.Itoa(r.Sessions), strconv.Itoa(r.Messages),
			strconv.FormatInt(r.Tokens, 10), strconv.FormatInt(r.CacheRead, 10),
			strconv.FormatFloat(r.CostUSD, 'f', -1, 64),
			strconv.FormatBool(r.Estimated),
		})
	}
	modelRecords := [][]string{{"model", "source", "sessions",
		"tokens", "cache_read", "cost_usd", "estimated"}}
	for _, m := range models {
		modelRecords = append(modelRecords, []string{
			m.Model, m.Source, strconv.Itoa(m.Sessions),
			strconv.FormatInt(m.Tokens, 10), strconv.FormatInt(m.CacheRead, 10),
			strconv.FormatFloat(m.CostUSD, 'f', -1, 64),
			strconv.FormatBool(m.Estimated),
		})
	}
	for _, out := range []struct {
		path    string
		records [][]string
	}{
		{prefix + "projects.csv", projRecords},
		{prefix + "models.csv", modelRecords},
	} {
		if err := writeCSV(out.path, out.records); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "%s\t%d rows\n", out.path, len(out.records)-1)
	}
	return nil
}

func writeCSV(path string, records [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.WriteAll(records); err != nil { // WriteAll flushes
		return err
	}
	return f.Close()
}

// fmtTokens renders a token count compactly (1.2M, 340k). est prefixes a ~.
func fmtTokens(n int64, est bool) string {
	if n == 0 {
		return "-"
	}
	mark := ""
	if est {
		mark = "~"
	}
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%s%.1fB", mark, float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%s%.1fM", mark, float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%s%.0fk", mark, float64(n)/1e3)
	}
	return fmt.Sprintf("%s%d", mark, n)
}

func fmtCost(usd float64) string {
	if usd == 0 {
		return "-"
	}
	if usd < 1 {
		return fmt.Sprintf("%.0f\u00a2", usd*100)
	}
	return fmt.Sprintf("$%.2f", usd)
}

func runRelated(args []string) error {
	fs := flag.NewFlagSet("related", flag.ExitOnError)
	var cf commonFlags
	cf.register(fs)
	flagArgs, posArgs := splitFlagsAndArgs(args, sharedBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(posArgs) == 0 {
		return errors.New("usage: recall related <session-id>")
	}
	id := posArgs[0]
	limit := 10
	if cf.limit > 0 {
		limit = cf.limit
	}
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()
	hits, err := ix.Related(id, limit)
	if err != nil {
		return err
	}
	if cf.json {
		return JSONNewEncoder(os.Stdout).Encode(hits)
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stdout, "no related sessions found")
		return nil
	}
	printHits(os.Stdout, hits)
	return nil
}

func runSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	var cf commonFlags
	cf.register(fs)
	flagArgs, _ := splitFlagsAndArgs(args, sharedBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	opts, err := cf.toSearchOpts()
	if err != nil {
		return err
	}

	if cf.repo == "" && cf.since == "" && opts.Source == "" && len(opts.Tags) == 0 {
		if cwd, err := os.Getwd(); err == nil {
			opts.Project = resolveRepo(cwd)
		}
	}
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()
	hits, err := ix.Search("", opts)
	if err != nil {
		return err
	}
	if cf.json {
		return JSONNewEncoder(os.Stdout).Encode(hits)
	}
	for _, h := range hits {
		when := h.StartedTime().Format("2006-01-02 15:04")
		fmt.Printf("%s  %-6s  %-30s  %s\n",
			when, h.Source, truncate(shortProject(h.Project), 30), truncate(h.Title, 60))
		fmt.Printf("  id=%s\n", h.SessionID)
	}
	printPager(os.Stdout, "sessions", "", cf, hits)
	return nil
}
