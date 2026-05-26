package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const version = "0.1.0"

func usage() {
	fmt.Fprintf(os.Stderr, `recall %s — your AI chat history, searchable across Cursor, Claude Code, Codex.

USAGE
  recall index                       (re)build the index from all sources
  recall <query>                     search; prints ranked hits
  recall find <query> [--repo P]     same, with filters
  recall last [--repo P]             print the most recent session as transcript
  recall show <session-id>           print one session as transcript (full fidelity)
  recall sessions [--repo P]         list recent sessions
  recall open <session-id> [--print] reopen the chat in its source tool
  recall doctor                      health check
  recall version

FLAGS
  --repo PATH      restrict to a specific project folder
  --source NAME    cursor | claude | codex
  --since DURATION e.g. 24h, 7d
  --limit N        default 30
  --json           machine-readable output

DATA
  index lives at  ~/.recall/index.sqlite      (disposable; rebuild any time)
  sources read    ~/Library/Application Support/Cursor/User/{global,workspace}Storage/state.vscdb
                  ~/.claude/projects/*/*.jsonl
                  ~/.codex/sessions/**/rollout-*.jsonl
`, version)
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
		fmt.Println(version)
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
	default:
		// Implicit search: `recall <query…> [flags…]`. Pass argv through;
		// runFind separates flags from positionals.
		if err := runFind(os.Args[1:]); err != nil {
			fatal(err)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "recall: "+err.Error())
	os.Exit(1)
}

// ---------- shared helpers ----------

func defaultIndexPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".recall", "index.sqlite")
}

func defaultAdapters() []Adapter {
	home, _ := os.UserHomeDir()
	return []Adapter{
		&CursorAdapter{UserDir: filepath.Join(home, "Library", "Application Support", "Cursor", "User")},
		&ClaudeAdapter{Root: filepath.Join(home, ".claude", "projects")},
		&CodexAdapter{Root: filepath.Join(home, ".codex", "sessions")},
		&PiAdapter{Root: filepath.Join(home, ".pi", "agent", "sessions")},
	}
}

type commonFlags struct {
	repo   string
	source string
	since  string
	limit  int
	json   bool
}

func (cf *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&cf.repo, "repo", "", "restrict to a project folder")
	fs.StringVar(&cf.source, "source", "", "cursor | claude | codex")
	fs.StringVar(&cf.since, "since", "", "e.g. 24h, 7d, 30d")
	fs.IntVar(&cf.limit, "limit", 30, "max results")
	fs.BoolVar(&cf.json, "json", false, "machine-readable output")
}

// splitFlagsAndArgs lets flags appear anywhere on the line.
// We treat any token starting with "-" as a flag, and consume the next token as
// its value unless it also starts with "-" or "=" is embedded.
func splitFlagsAndArgs(in []string, boolFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(in); i++ {
		a := in[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// Has "=value" inline? done.
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

var sharedBoolFlags = map[string]bool{"json": true}

func (cf *commonFlags) toSearchOpts() (SearchOpts, error) {
	opts := SearchOpts{
		Source:  cf.source,
		Project: cf.repo,
		Limit:   cf.limit,
	}
	if cf.since != "" {
		d, err := parseSince(cf.since)
		if err != nil {
			return opts, err
		}
		opts.Since = time.Now().Add(-d).UnixMilli()
	}
	return opts, nil
}

func parseSince(s string) (time.Duration, error) {
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
	return openIndex(path)
}

// ---------- commands ----------

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	full := fs.Bool("full", false, "ignore checkpoints and reindex everything")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ix, err := openIndex(defaultIndexPath())
	if err != nil {
		return err
	}
	defer ix.Close()

	start := time.Now()
	var grand int
	for _, ad := range defaultAdapters() {
		if !ad.Available() {
			fmt.Printf("  %-7s skipped (not installed)\n", ad.ID())
			continue
		}
		t0 := time.Now()
		var prev string
		if !*full {
			prev = ix.GetMeta("ckpt:" + ad.ID())
		}
		sessions, msgs, next, err := ad.Scan(prev)
		if err != nil {
			fmt.Printf("  %-7s error: %v\n", ad.ID(), err)
			continue
		}
		if err := ix.IngestBatch(ad.ID(), sessions, msgs); err != nil {
			fmt.Printf("  %-7s ingest error: %v\n", ad.ID(), err)
			continue
		}
		if next != "" {
			_ = ix.SetMeta("ckpt:"+ad.ID(), next)
		}
		grand += len(sessions)
		fmt.Printf("  %-7s %6d sessions  %7d messages  %s\n",
			ad.ID(), len(sessions), len(msgs), time.Since(t0).Round(time.Millisecond))
	}
	_ = ix.SetMeta("indexed_at", time.Now().Format(time.RFC3339))
	fmt.Printf("indexed %d sessions in %s → %s\n",
		grand, time.Since(start).Round(time.Millisecond), defaultIndexPath())
	return nil
}

func runDoctor(_ []string) error {
	fmt.Printf("recall %s\n\n", version)
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
	ix, err := openIndex(idxPath)
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
	// Iterate adapters in their default order so output stays stable across runs.
	for _, ad := range defaultAdapters() {
		if c, ok := counts[ad.ID()]; ok {
			fmt.Printf("  %-7s %d sessions\n", ad.ID(), c)
			total += c
		}
	}
	// Any source rows from older runs we no longer have an adapter for.
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
	defer ix.Close()
	hits, err := ix.Search(query, opts)
	if err != nil {
		return err
	}
	if cf.json {
		return JSONNewEncoder(os.Stdout).Encode(hits)
	}
	printHits(os.Stdout, hits)
	return nil
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
	flagArgs, _ := splitFlagsAndArgs(args, sharedBoolFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	opts, _ := cf.toSearchOpts()
	opts.Limit = 1
	if opts.Project == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.Project = cwd
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
	return printTranscript(ix, hits[0].SessionID, cf.json)
}

func runShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: recall show <session-id>")
	}
	ix, err := openIndexOrFail()
	if err != nil {
		return err
	}
	defer ix.Close()
	return printTranscript(ix, fs.Arg(0), *asJSON)
}

func printTranscript(ix *Index, id string, asJSON bool) error {
	s, err := ix.LookupSession(id)
	if err != nil {
		return fmt.Errorf("session %s: %w", id, err)
	}
	// Re-read from source for full fidelity (no excerpt truncation).
	ad := adapterFor(s.Source)
	if ad == nil {
		return fmt.Errorf("no adapter for source %q", s.Source)
	}
	msgs, err := ad.Fetch(s.SourceID)
	if err != nil {
		return err
	}
	if asJSON {
		return JSONNewEncoder(os.Stdout).Encode(map[string]any{
			"session":  s,
			"messages": msgs,
		})
	}
	fmt.Printf("# %s\n", s.Title)
	fmt.Printf("source=%s  project=%s  started=%s  msgs=%d\n\n",
		s.Source, shortProject(s.Project),
		time.UnixMilli(s.StartedAt).Format(time.RFC3339), s.MsgCount)
	for _, m := range msgs {
		fmt.Printf("## %s\n%s\n\n", m.Role, m.Text)
	}
	return nil
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
		// macOS: open <uri> hands off to URL handler
		return runShell("open", target)
	default:
		// CLI invocation form (claude --resume <id>, codex resume <id>) — just print.
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
	// Default to current repo only if user gave no other filter.
	if cf.repo == "" && cf.source == "" && cf.since == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.Project = cwd
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
	return nil
}
