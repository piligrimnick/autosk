// toolFormatters — per-tool formatter registry for the collapsible ToolBlock
// (plan docs/plans/20260628-gui-transcript-folding.md §8). Each formatter is a
// mostly-pure helper: `summary(args)` builds the collapsed-header summary (a
// primary argument + muted hints, rendered after the bold tool name);
// `renderArgs` / `renderResult` are optional React overrides for the expanded
// body (ToolBlock supplies plain-text defaults — no syntax highlighting / diff
// this iteration, decision §3.9). Textual core set + a generic fallback.

import type { ReactNode } from "react";
import type { ToolResultMessage } from "@/types";

/** The collapsed-header summary: a primary arg (accent) + muted hint string. */
export interface ToolSummary {
  primary?: string;
  hint?: string;
}

export interface ToolFormatter {
  /** Pure + testable: the collapsed-header summary shown after the tool name. */
  summary(args: Record<string, unknown>): ToolSummary;
  /** Optional expanded args body. Default (in ToolBlock): pretty-printed JSON. */
  renderArgs?(args: Record<string, unknown>): ReactNode;
  /** Optional expanded result body. Default (in ToolBlock): text/image join. */
  renderResult?(result: ToolResultMessage): ReactNode;
}

// ---- pure string helpers --------------------------------------------------

function asStr(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return "";
}

function truncate(s: string, max: number): string {
  const t = s.trim();
  if (t.length <= max) return t;
  return t.slice(0, Math.max(0, max - 1)).trimEnd() + "…";
}

function firstLine(s: string, max: number): string {
  const line = s.split("\n")[0] ?? "";
  return truncate(line, max);
}

function quoted(s: string, max: number): string | undefined {
  const t = truncate(s, max);
  return t ? `"${t}"` : undefined;
}

function joinHints(parts: (string | undefined | false)[]): string | undefined {
  const out = parts.filter((p): p is string => !!p).join(" ");
  return out || undefined;
}

// ---- the core-set registry (decision §3.9) --------------------------------

const FORMATTERS: Record<string, ToolFormatter> = {
  read: {
    summary(args) {
      return {
        primary: asStr(args.path) || undefined,
        hint: joinHints([
          args.offset != null && `from line ${asStr(args.offset)}`,
          args.limit != null && `(${asStr(args.limit)} lines)`,
        ]),
      };
    },
  },
  bash: {
    summary(args) {
      return { primary: firstLine(asStr(args.command), 100) || undefined };
    },
  },
  grep: {
    summary(args) {
      return {
        primary: quoted(asStr(args.pattern), 60),
        hint: joinHints([
          args.path ? `in ${asStr(args.path)}` : undefined,
          args.glob ? `(${asStr(args.glob)})` : undefined,
        ]),
      };
    },
  },
  find: {
    summary(args) {
      return {
        primary: asStr(args.pattern) || undefined,
        hint: args.path ? `in ${asStr(args.path)}` : undefined,
      };
    },
  },
  ls: {
    summary(args) {
      return { primary: asStr(args.path) || undefined };
    },
  },
  write: {
    summary(args) {
      return { primary: asStr(args.path) || undefined };
    },
  },
  edit: {
    summary(args) {
      const n = Array.isArray(args.edits) ? args.edits.length : 0;
      return {
        primary: asStr(args.path) || undefined,
        hint: n ? `(${n} edit${n === 1 ? "" : "s"})` : undefined,
      };
    },
  },
  web_search: {
    summary(args) {
      const loc = args.user_location as { country?: unknown } | undefined;
      const country = loc && typeof loc === "object" ? asStr(loc.country) : "";
      return {
        primary: quoted(asStr(args.query), 60),
        hint: joinHints([
          Array.isArray(args.allowed_domains) && args.allowed_domains.length
            ? `+${args.allowed_domains.length}d`
            : undefined,
          Array.isArray(args.blocked_domains) && args.blocked_domains.length
            ? `-${args.blocked_domains.length}d`
            : undefined,
          args.max_uses != null ? `max=${asStr(args.max_uses)}` : undefined,
          country || undefined,
        ]),
      };
    },
  },
  web_fetch: {
    summary(args) {
      const pages = Array.isArray(args.pages) ? args.pages.length : 0;
      const primary =
        asStr(args.url) || (pages ? `${pages} page${pages === 1 ? "" : "s"}` : undefined);
      return {
        primary,
        hint: asStr(args.prompt) ? truncate(asStr(args.prompt), 60) : undefined,
      };
    },
  },
};

/** The generic fallback for unknown tools: the bold name only, no summary. */
const GENERIC: ToolFormatter = {
  summary() {
    return {};
  },
};

/** Registry lookup (case-insensitive) with the generic fallback. */
export function formatterFor(name: string): ToolFormatter {
  return FORMATTERS[(name ?? "").toLowerCase()] ?? GENERIC;
}
