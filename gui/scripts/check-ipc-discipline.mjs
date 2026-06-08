#!/usr/bin/env node
// IPC discipline guard (plan §6, acceptance criterion):
//   * `src/services/ipc.ts` is the ONLY site that imports/uses Tauri `invoke`.
//   * `src/services/events.ts` is the ONLY site that imports/uses Tauri `listen`.
//
// This keeps the frontend transport-agnostic: every command crosses the bridge
// through one typed chokepoint, and every server push fans out through one
// ref-counted hub. Run from `npm run typecheck` (and CI) so a stray
// `invoke`/`listen` import fails the build loudly.

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const srcDir = join(root, "src");

const ALLOWED = {
  invoke: "src/services/ipc.ts",
  listen: "src/services/events.ts",
};

/** Recursively collect .ts/.tsx files under dir. */
function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const st = statSync(full);
    if (st.isDirectory()) {
      out.push(...walk(full));
    } else if (/\.tsx?$/.test(entry)) {
      out.push(full);
    }
  }
  return out;
}

// Match an import of `invoke` from @tauri-apps/api/core (named or aliased) and
// any bare `invoke(` / `listen(` call site outside the allowed files.
const IMPORT_RE = {
  invoke: /from\s+["']@tauri-apps\/api(\/core)?["']/,
  listen: /from\s+["']@tauri-apps\/api\/event["']/,
};

const violations = [];
for (const file of walk(srcDir)) {
  const rel = relative(root, file).split("\\").join("/");
  const text = readFileSync(file, "utf8");
  for (const sym of ["invoke", "listen"]) {
    if (rel === ALLOWED[sym]) {
      continue;
    }
    // Flag a named import of the symbol from the tauri api…
    const importsSymbol =
      IMPORT_RE[sym].test(text) && new RegExp(`\\b${sym}\\b`).test(text);
    if (importsSymbol) {
      violations.push(
        `${rel}: imports Tauri \`${sym}\` — only ${ALLOWED[sym]} may.`,
      );
    }
  }
}

if (violations.length > 0) {
  console.error("IPC discipline check FAILED:\n  " + violations.join("\n  "));
  console.error(
    "\nAll Tauri `invoke` calls must go through src/services/ipc.ts;",
  );
  console.error("all Tauri `listen` calls through src/services/events.ts.");
  process.exit(1);
}

console.log("IPC discipline check passed (single invoke + single listen site).");
