/**
 * pi-recall: search your past AI chat history from inside pi.
 *
 * Shells out to the `recall` Go CLI (https://github.com/pratikgajjar/recall),
 * which indexes Cursor / Claude Code / Codex / pi conversations into a local
 * SQLite FTS5 index.
 *
 * This extension deliberately registers no tools. It installs the recall skill
 * and keeps the index warm; the agent then uses the CLI directly, the way a
 * person would. One surface, one set of flags, one thing to keep correct.
 */

import { execFile } from "node:child_process";
import { chmodSync, existsSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Resolve the prebuilt binary bundled in this package's bin/ directory. The
// release pipeline drops one binary per platform (recall-<os>-<arch>); we pick
// the one matching the host. Returns undefined when none is present (e.g. a
// source checkout), so the caller falls back to `recall` on PATH.
function resolveBundledBin(): string | undefined {
  const name = `recall-${process.platform}-${process.arch}`;
  const binPath = join(dirname(fileURLToPath(import.meta.url)), "..", "bin", name);
  if (!existsSync(binPath)) return undefined;
  // npm has historically lost the +x bit on extracted tarball binaries;
  // restore it best-effort so the first call succeeds.
  try {
    const mode = statSync(binPath).mode;
    if (!(mode & 0o111)) chmodSync(binPath, 0o755);
  } catch {
    /* best effort */
  }
  return binPath;
}
const EXEC_MAX_BUFFER = 32 * 1024 * 1024; // transcripts can be large

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
    // Precedence: explicit flag -> env override -> binary bundled in this
    // package's bin/ (shipped by the release pipeline) -> `recall` on PATH.
    const flag = pi.getFlag("recall-bin") as string | undefined;
    if (flag) return flag;
    if (process.env.RECALL_BIN) return process.env.RECALL_BIN;
    return resolveBundledBin() ?? "recall";
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
    // Keep the installed skill in step with this binary, then catch up on
    // anything that changed since the last pi session. The incremental index is
    // append-only, so this is cheap (~tens of ms).
    if (recallAvailable) {
      await installSkill();
      scheduleRefresh(0);
    }
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

  // Skills, not tools. recall is a CLI: the agent runs it the way a person
  // would, and the skill file tells it how. Registering tools meant a JSON
  // schema re-sent on every single turn whether or not recall was used, plus a
  // second surface to keep in step with the CLI — two descriptions of the same
  // flags, drifting apart. `recall skill install` writes the copy embedded in
  // the binary, so the instructions always match the binary that ships them.
  const installSkill = async () => {
    const res = await runRecall(["skill", "install"]);
    if (!res.ok) return;
  };

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
