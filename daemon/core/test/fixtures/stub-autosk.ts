#!/usr/bin/env bun
/**
 * `stub-autosk` — a test stand-in for the `autosk` Go CLI, used to exercise the
 * `autoskd mcp` server's `task` / `comment` shell-out without a real daemon. The
 * MCP server (`daemon/core/src/mcp/`) spawns `$AUTOSK_BIN` (which the test points
 * at this file) with the same argv `@autosk/pi-tools` builds and parses its
 * `--json` stdout.
 *
 * It emits canned `--json` keyed by the verb (`create` / `update` / `show` /
 * `list` / `comment add` / `comment list`). `comment add` defaults the author to
 * `$AUTOSK_AGENT` (or "human") when no `--author` is passed — mirroring the real
 * CLI, so the test can assert the env-default behaviour.
 */

const argv = process.argv.slice(2);
// Drop the trailing `--json` the MCP cli layer appends; the stub always emits JSON.
const args = argv.filter((a) => a !== "--json");

const NOW = "2026-06-18T00:00:00Z";

function task(overrides: Record<string, unknown> = {}): unknown {
  return {
    id: "ask-stub01",
    title: "stub task",
    description: "",
    status: "new",
    workflow: "",
    step: "",
    created_at: NOW,
    updated_at: NOW,
    blocked: false,
    blocked_by: [],
    blocks: [],
    comment_count: 0,
    ...overrides,
  };
}

function emit(obj: unknown): void {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

/** Reads the value following a flag in argv, or undefined. */
function flag(name: string): string | undefined {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
}

const verb = args[0];

if (verb === "create") {
  const title = args[1] ?? "";
  emit(task({ id: "ask-created", title, status: flag("--workflow") ? "work" : "new", workflow: flag("--workflow") ?? "", step: flag("--workflow") ? "dev" : "" }));
} else if (verb === "update") {
  emit(task({ id: args[1], title: flag("--title") ?? "stub task", description: flag("--description") ?? "" }));
} else if (verb === "show") {
  emit(task({ id: args[1], comment_count: 2 }));
} else if (verb === "list") {
  emit([task({ id: "ask-1", title: "one" }), task({ id: "ask-2", title: "two", status: "work" })]);
} else if (verb === "comment") {
  const sub = args[1];
  if (sub === "add") {
    const taskId = args[2];
    const text = args[3] ?? "";
    const author = flag("--author") ?? process.env.AUTOSK_AGENT ?? "human";
    emit({ id: "cm-stub01", author, text, created_at: NOW, updated_at: NOW });
    void taskId;
  } else if (sub === "list") {
    emit([
      { id: "cm-1", author: "dev", text: "first", created_at: NOW, updated_at: NOW },
      { id: "cm-2", author: "review", text: "second", created_at: NOW, updated_at: NOW },
    ]);
  } else {
    process.stderr.write(`stub-autosk: unknown comment subcommand: ${sub}\n`);
    process.exit(2);
  }
} else {
  process.stderr.write(`stub-autosk: unknown verb: ${verb}\n`);
  process.exit(2);
}
