/**
 * pi-recall: search your past AI chat history from inside pi.
 *
 * Shells out to the `recall` Go CLI (https://github.com/pratikgajjar/recall),
 * which indexes Cursor / Claude Code / Codex / pi conversations into a local
 * SQLite FTS5 index. This extension surfaces that index to the agent as tools
 * so it can recall prior work without you copy-pasting transcripts.
 */

import { execFile } from "node:child_process";
import { homedir } from "node:os";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Text } from "@earendil-works/pi-tui";
import { Type } from "typebox";

const DEFAULT_SEARCH_LIMIT = 15;
const DEFAULT_SESSIONS_LIMIT = 20;
const DEFAULT_RELATED_LIMIT = 10;
const TRANSCRIPT_MAX_LINES = 500;
const EXEC_MAX_BUFFER = 32 * 1024 * 1024; // transcripts can be large

const SOURCE_HELP = "Restrict to one tool: cursor | claude | codex | pi.";
const REPO_HELP =
  "Restrict to a project folder. Pass '.' for the current working directory.";
const SINCE_HELP = "Only sessions newer than this, e.g. '24h', '7d', '30d'.";

interface RecallHit {
  session_id: string;
  source: string;
  source_id: string;
  project: string;
  title: string;
  started_at_ms: number;
  msg_idx: number;
  role: string;
  snippet: string;
  rank: number;
}

interface RunResult {
  ok: boolean;
  stdout: string;
  stderr: string;
  code: number | null;
}

const REFRESH_DEBOUNCE_MS = 1500;

export default function recallExtension(pi: ExtensionAPI) {
  let activeCwd = process.cwd();
  let warnedMissing = false;

  // Background-refresh state. A pi extension is a long-lived process, so we
  // keep the recall index warm out-of-band (on session start + after each
  // agent turn) instead of paying an incremental rebuild on the query path.
  let recallAvailable = false;
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;
  let refreshing = false;

  function autoIndexEnabled(): boolean {
    const flag = pi.getFlag("recall-auto-index") as boolean | undefined;
    if (flag === false) return false;
    const env = process.env.RECALL_AUTO_INDEX;
    if (env === "0" || env === "false" || env === "off") return false;
    return true;
  }

  async function runRefresh(): Promise<void> {
    refreshTimer = undefined;
    if (!recallAvailable || !autoIndexEnabled()) return;
    if (refreshing) {
      scheduleRefresh(500); // one already in flight — retry shortly
      return;
    }
    refreshing = true;
    try {
      await runRecall(["index"]);
    } catch {
      // best-effort; a failed background refresh just leaves the index as-is
    } finally {
      refreshing = false;
    }
  }

  function scheduleRefresh(delayMs = REFRESH_DEBOUNCE_MS): void {
    if (!recallAvailable || !autoIndexEnabled()) return;
    if (refreshTimer) clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => void runRefresh(), delayMs);
  }

  function resolveBin(): string {
    return (
      (pi.getFlag("recall-bin") as string | undefined) ??
      process.env.RECALL_BIN ??
      "recall"
    );
  }

  function runRecall(args: string[], signal?: AbortSignal): Promise<RunResult> {
    const bin = resolveBin();
    return new Promise((resolve) => {
      execFile(
        bin,
        args,
        { maxBuffer: EXEC_MAX_BUFFER, signal },
        (err, stdout, stderr) => {
          if (err && (err as NodeJS.ErrnoException).code === "ENOENT") {
            resolve({
              ok: false,
              stdout: "",
              stderr: `recall binary not found ('${bin}'). Install it: go install github.com/pratikgajjar/recall@latest`,
              code: 127,
            });
            return;
          }
          const code =
            err && typeof (err as { code?: unknown }).code === "number"
              ? ((err as { code: number }).code as number)
              : err
                ? 1
                : 0;
          resolve({
            ok: !err,
            stdout: stdout ?? "",
            stderr: (stderr ?? "").trim(),
            code,
          });
        },
      );
    });
  }

  // --- formatting helpers ---

  function shortenPath(p: string): string {
    if (!p) return "";
    const home = homedir();
    return p.startsWith(home) ? `~${p.slice(home.length)}` : p;
  }

  function fmtDate(ms: number): string {
    if (!ms) return "";
    const d = new Date(ms);
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }

  function resolveRepo(repo?: string): string | undefined {
    if (!repo) return undefined;
    return repo === "." ? activeCwd : repo;
  }

  function buildFilterArgs(params: {
    repo?: string;
    source?: string;
    since?: string;
    limit?: number;
  }): string[] {
    const args: string[] = [];
    const repo = resolveRepo(params.repo);
    if (repo) args.push("--repo", repo);
    if (params.source) args.push("--source", params.source);
    if (params.since) args.push("--since", params.since);
    if (params.limit !== undefined)
      args.push("--limit", String(Math.max(1, params.limit)));
    return args;
  }

  function parseHits(stdout: string): RecallHit[] {
    const trimmed = stdout.trim();
    if (!trimmed || trimmed === "null") return [];
    try {
      const parsed = JSON.parse(trimmed);
      return Array.isArray(parsed) ? (parsed as RecallHit[]) : [];
    } catch {
      return [];
    }
  }

  function formatHits(hits: RecallHit[], showSnippet: boolean): string {
    if (hits.length === 0) return "No matching sessions found.";
    const blocks = hits.map((h) => {
      const head = [fmtDate(h.started_at_ms), h.source, shortenPath(h.project)]
        .filter(Boolean)
        .join("  ");
      const lines = [head, `  ${h.title || "(untitled)"}   id=${h.session_id}`];
      if (showSnippet && h.snippet) lines.push(`  ${h.snippet.trim()}`);
      return lines.join("\n");
    });
    return blocks.join("\n\n");
  }

  function capTranscript(text: string): string {
    const lines = text.split("\n");
    if (lines.length <= TRANSCRIPT_MAX_LINES) return text;
    const shown = lines.slice(0, TRANSCRIPT_MAX_LINES).join("\n");
    const more = lines.length - TRANSCRIPT_MAX_LINES;
    return `${shown}\n\n[truncated — ${more} more lines. Open the full session in its tool with: recall open <id>]`;
  }

  // --- lifecycle ---

  pi.registerFlag("recall-bin", {
    description: "Path to the recall binary (overrides RECALL_BIN env; default: 'recall' on PATH)",
    type: "string",
  });

  pi.registerFlag("recall-auto-index", {
    description:
      "Keep the recall index fresh in the background (session start + after each turn). Default: on. Disable with --recall-auto-index=false or RECALL_AUTO_INDEX=0.",
    type: "boolean",
  });

  pi.on("session_start", async (_event, ctx) => {
    activeCwd = ctx.cwd;
    const res = await runRecall(["version"]);
    recallAvailable = res.ok;
    if (!res.ok && !warnedMissing) {
      warnedMissing = true;
      ctx.ui.notify(
        `pi-recall: ${res.stderr || "recall CLI not available"}`,
        "warning",
      );
    }
    // Catch up on anything that changed since the last pi session. The
    // incremental index is append-only, so this is cheap (~tens of ms).
    if (recallAvailable) scheduleRefresh(0);
  });

  // After each agent response the current session's transcript has grown.
  // Debounce a background refresh so the index reflects it before the next
  // recall_search — the only file actively changing is this one, and we know
  // exactly when it does. Tool calls then read an already-fresh index.
  pi.on("agent_end", async () => {
    scheduleRefresh();
  });

  pi.on("session_shutdown", async () => {
    if (refreshTimer) {
      clearTimeout(refreshTimer);
      refreshTimer = undefined;
    }
  });

  // --- shared render helpers ---

  const renderTextResult = (
    result: { content?: { type: string; text?: string }[] },
    options: { expanded?: boolean },
    theme: any,
    context: any,
    maxLines = 15,
  ) => {
    const text = (context.lastComponent as Text | undefined) ?? new Text("", 0, 0);
    const output = result.content?.find((c) => c.type === "text")?.text?.trim() ?? "";
    if (!output) {
      text.setText(theme.fg("muted", "No output"));
      return text;
    }
    const lines = output.split("\n");
    const display = lines.slice(0, options.expanded ? lines.length : maxLines);
    let content = `\n${display.map((l: string) => theme.fg("toolOutput", l)).join("\n")}`;
    if (lines.length > display.length)
      content += theme.fg("muted", `\n... (${lines.length - display.length} more lines)`);
    text.setText(content);
    return text;
  };

  // --- recall_search ---

  const searchSchema = Type.Object({
    query: Type.String({
      description:
        "Full-text search over your past AI conversations (titles + message excerpts). Use concrete identifiers, error strings, or feature names.",
    }),
    repo: Type.Optional(Type.String({ description: REPO_HELP })),
    source: Type.Optional(Type.String({ description: SOURCE_HELP })),
    since: Type.Optional(Type.String({ description: SINCE_HELP })),
    limit: Type.Optional(
      Type.Number({ description: `Max hits (default ${DEFAULT_SEARCH_LIMIT})` }),
    ),
  });

  pi.registerTool({
    name: "recall_search",
    label: "recall search",
    description:
      "Search your own past AI chat history across Cursor, Claude Code, Codex, and pi. Returns ranked sessions with matched excerpts and a session id. Use recall_transcript to read a hit in full.",
    promptSnippet: "Search past AI conversations across tools",
    promptGuidelines: [
      "Use recall_search when the user references earlier work ('how did we fix…', 'what did I decide about…', 'continue the…') that may live in a prior chat.",
      "Pass repo: '.' to scope recall_search to the current project.",
      "After recall_search, call recall_transcript with the returned id to read the full session before acting.",
    ],
    parameters: searchSchema,

    async execute(_id, params, signal) {
      const args = ["find", params.query, "--json", ...buildFilterArgs({
        repo: params.repo,
        source: params.source,
        since: params.since,
        limit: params.limit ?? DEFAULT_SEARCH_LIMIT,
      })];
      const res = await runRecall(args, signal);
      if (!res.ok) throw new Error(res.stderr || `recall exited with code ${res.code}`);
      const hits = parseHits(res.stdout);
      return {
        content: [{ type: "text", text: formatHits(hits, true) }],
        details: { count: hits.length },
      };
    },

    renderCall(args, theme, context) {
      const text = (context.lastComponent as Text | undefined) ?? new Text("", 0, 0);
      let c =
        theme.fg("toolTitle", theme.bold("recall search")) +
        " " +
        theme.fg("accent", `"${args?.query ?? ""}"`);
      if (args?.repo) c += theme.fg("toolOutput", ` in ${args.repo}`);
      if (args?.source) c += theme.fg("muted", ` [${args.source}]`);
      text.setText(c);
      return text;
    },
    renderResult(result, options, theme, context) {
      return renderTextResult(result, options, theme, context, 15);
    },
  });

  // --- recall_transcript ---

  const transcriptSchema = Type.Object({
    session_id: Type.Optional(
      Type.String({
        description:
          "Session id from recall_search/recall_sessions (e.g. 'cursor:…', 'pi:…'). Omit to fetch the most recent session matching the filters below.",
      }),
    ),
    repo: Type.Optional(Type.String({ description: REPO_HELP })),
    source: Type.Optional(Type.String({ description: SOURCE_HELP })),
    since: Type.Optional(Type.String({ description: SINCE_HELP })),
  });

  pi.registerTool({
    name: "recall_transcript",
    label: "recall transcript",
    description:
      "Read a past AI session as a full transcript. Pass a session_id from recall_search, or omit it to get the most recent session (optionally filtered by repo/source/since).",
    promptSnippet: "Read a past AI session transcript",
    promptGuidelines: [
      "Call recall_transcript after recall_search to read a specific session before reusing its decisions.",
      "Omit session_id with repo: '.' to pull the most recent conversation in this project.",
    ],
    parameters: transcriptSchema,

    async execute(_id, params, signal) {
      const args = params.session_id
        ? ["show", params.session_id]
        : ["last", ...buildFilterArgs({
            repo: params.repo,
            source: params.source,
            since: params.since,
          })];
      const res = await runRecall(args, signal);
      if (!res.ok) {
        const msg = res.stderr || res.stdout.trim() || `recall exited with code ${res.code}`;
        return {
          content: [{ type: "text", text: msg }],
          details: { found: false },
          isError: true,
        };
      }
      return {
        content: [{ type: "text", text: capTranscript(res.stdout.trim()) }],
        details: { found: true },
      };
    },

    renderCall(args, theme, context) {
      const text = (context.lastComponent as Text | undefined) ?? new Text("", 0, 0);
      const target = args?.session_id ?? (args?.repo ? `last in ${args.repo}` : "last");
      text.setText(
        theme.fg("toolTitle", theme.bold("recall transcript")) +
          " " +
          theme.fg("accent", target),
      );
      return text;
    },
    renderResult(result, options, theme, context) {
      return renderTextResult(result, options, theme, context, 20);
    },
  });

  // --- recall_sessions ---

  const sessionsSchema = Type.Object({
    repo: Type.Optional(Type.String({ description: REPO_HELP })),
    source: Type.Optional(Type.String({ description: SOURCE_HELP })),
    since: Type.Optional(Type.String({ description: SINCE_HELP })),
    limit: Type.Optional(
      Type.Number({ description: `Max sessions (default ${DEFAULT_SESSIONS_LIMIT})` }),
    ),
  });

  pi.registerTool({
    name: "recall_sessions",
    label: "recall sessions",
    description:
      "List recent past AI sessions (titles + ids, no bodies). Filter by repo/source/since. Use to browse what you've worked on, then recall_transcript to open one.",
    promptSnippet: "List recent past AI sessions",
    promptGuidelines: [
      "Use recall_sessions with repo: '.' to see recent prior conversations in the current project.",
    ],
    parameters: sessionsSchema,

    async execute(_id, params, signal) {
      const args = ["sessions", "--json", ...buildFilterArgs({
        repo: params.repo,
        source: params.source,
        since: params.since,
        limit: params.limit ?? DEFAULT_SESSIONS_LIMIT,
      })];
      const res = await runRecall(args, signal);
      if (!res.ok) throw new Error(res.stderr || `recall exited with code ${res.code}`);
      const hits = parseHits(res.stdout);
      return {
        content: [{ type: "text", text: formatHits(hits, false) }],
        details: { count: hits.length },
      };
    },

    renderCall(args, theme, context) {
      const text = (context.lastComponent as Text | undefined) ?? new Text("", 0, 0);
      let c = theme.fg("toolTitle", theme.bold("recall sessions"));
      if (args?.repo) c += theme.fg("toolOutput", ` in ${args.repo}`);
      if (args?.source) c += theme.fg("muted", ` [${args.source}]`);
      text.setText(c);
      return text;
    },
    renderResult(result, options, theme, context) {
      return renderTextResult(result, options, theme, context, 20);
    },
  });

  // --- recall_related ---

  const relatedSchema = Type.Object({
    session_id: Type.String({
      description: "Session id to find topically-similar sessions for.",
    }),
    limit: Type.Optional(
      Type.Number({ description: `Max neighbours (default ${DEFAULT_RELATED_LIMIT})` }),
    ),
  });

  pi.registerTool({
    name: "recall_related",
    label: "recall related",
    description:
      "Given a session id, find other past sessions covering the same topic. Useful to gather all prior work on a problem.",
    promptSnippet: "Find sessions related to a given one",
    promptGuidelines: [
      "Use recall_related after recall_search to widen context to neighbouring conversations on the same topic.",
    ],
    parameters: relatedSchema,

    async execute(_id, params, signal) {
      const args = [
        "related",
        params.session_id,
        "--json",
        "--limit",
        String(Math.max(1, params.limit ?? DEFAULT_RELATED_LIMIT)),
      ];
      const res = await runRecall(args, signal);
      if (!res.ok) throw new Error(res.stderr || `recall exited with code ${res.code}`);
      const hits = parseHits(res.stdout);
      return {
        content: [{ type: "text", text: formatHits(hits, true) }],
        details: { count: hits.length },
      };
    },

    renderCall(args, theme, context) {
      const text = (context.lastComponent as Text | undefined) ?? new Text("", 0, 0);
      text.setText(
        theme.fg("toolTitle", theme.bold("recall related")) +
          " " +
          theme.fg("accent", args?.session_id ?? ""),
      );
      return text;
    },
    renderResult(result, options, theme, context) {
      return renderTextResult(result, options, theme, context, 15);
    },
  });

  // --- commands ---

  pi.registerCommand("recall-health", {
    description: "Show recall CLI health and detected sources (recall doctor)",
    handler: async (_args, ctx) => {
      const res = await runRecall(["doctor"]);
      ctx.ui.notify(
        res.ok ? res.stdout.trim() : `recall doctor failed: ${res.stderr}`,
        res.ok ? "info" : "error",
      );
    },
  });

  pi.registerCommand("recall-index", {
    description: "Rebuild the recall index from all sources (recall index)",
    handler: async (args, ctx) => {
      ctx.ui.notify("recall: indexing…", "info");
      const indexArgs = (args || "").trim() === "--full" ? ["index", "--full"] : ["index"];
      const res = await runRecall(indexArgs);
      ctx.ui.notify(
        res.ok ? res.stdout.trim() || "recall index complete" : `recall index failed: ${res.stderr}`,
        res.ok ? "info" : "error",
      );
    },
  });
}
