/**
 * Login-shell PATH enrichment (`rpc/path-env.ts`): the "launched from a GUI app"
 * fix. Covers the pure parse/merge logic deterministically, plus a POSIX smoke
 * of the real login-shell probe.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter as PATH_DELIM, join } from "node:path";

import {
  computeEnrichedPath,
  mergePathEntries,
  parseShellPathOutput,
  queryLoginShellPath,
} from "../src/rpc/path-env.ts";

describe("mergePathEntries", () => {
  const sep = ":";

  test("login-shell entries win precedence, current entries are preserved", () => {
    const current = "/usr/bin:/bin";
    const shell = "/opt/homebrew/bin:/usr/bin:/bin";
    expect(mergePathEntries(current, shell, sep)).toBe("/opt/homebrew/bin:/usr/bin:/bin");
  });

  test("keeps a current-only entry the shell did not list (e.g. the daemon's own dir)", () => {
    const current = "/usr/bin:/Applications/autosk.app/Contents/Resources";
    const shell = "/opt/homebrew/bin:/usr/bin";
    expect(mergePathEntries(current, shell, sep)).toBe(
      "/opt/homebrew/bin:/usr/bin:/Applications/autosk.app/Contents/Resources",
    );
  });

  test("drops blank and duplicate entries", () => {
    expect(mergePathEntries("/a::/b:/a", "/b::/c", sep)).toBe("/b:/c:/a");
  });

  test("shell-first union reorders when shell precedence differs", () => {
    expect(mergePathEntries("/usr/bin:/opt/homebrew/bin", "/opt/homebrew/bin", sep)).toBe(
      "/opt/homebrew/bin:/usr/bin",
    );
  });
});

describe("computeEnrichedPath", () => {
  const sep = ":";

  test("reports the count of genuinely new dirs and applies shell precedence", () => {
    const current = "/usr/bin:/bin:/usr/sbin:/sbin";
    const shell = "/opt/homebrew/bin:/Users/me/.bun/bin:/usr/bin:/bin";
    const { path, added } = computeEnrichedPath(current, shell, sep);
    expect(added).toBe(2); // /opt/homebrew/bin + /Users/me/.bun/bin
    expect(path).toBe("/opt/homebrew/bin:/Users/me/.bun/bin:/usr/bin:/bin:/usr/sbin:/sbin");
  });

  test("added === 0 when the shell contributes no new dir (terminal-launched daemon)", () => {
    const current = "/opt/homebrew/bin:/usr/bin:/bin";
    const { added } = computeEnrichedPath(current, "/usr/bin:/opt/homebrew/bin", sep);
    expect(added).toBe(0);
  });
});

describe("parseShellPathOutput", () => {
  const wrap = (p: string) => `__AUTOSK_PATH__${p}__AUTOSK_PATH__`;

  test("extracts the PATH between the sentinels", () => {
    expect(parseShellPathOutput(wrap("/opt/homebrew/bin:/usr/bin"))).toBe("/opt/homebrew/bin:/usr/bin");
  });

  test("tolerates rc-file noise printed before the markers", () => {
    expect(parseShellPathOutput(`welcome!\nupdate available\n${wrap("/usr/bin:/bin")}`)).toBe("/usr/bin:/bin");
  });

  test("returns null when the markers are missing", () => {
    expect(parseShellPathOutput("no markers here")).toBeNull();
  });

  test("returns null for an empty PATH between markers", () => {
    expect(parseShellPathOutput(wrap(""))).toBeNull();
  });
});

describe("queryLoginShellPath", () => {
  // POSIX-only smoke: every dev/CI host has a working /bin/sh with /usr/bin on
  // PATH. The probe must fish a real PATH out of the login shell.
  test.skipIf(process.platform === "win32")("probes the real login shell PATH", async () => {
    const result = await queryLoginShellPath({ shell: "/bin/sh" });
    expect(result).not.toBeNull();
    expect(result!.split(PATH_DELIM)).toContain("/usr/bin");
  });

  const tmpDirs: string[] = [];
  afterEach(() => {
    for (const d of tmpDirs.splice(0)) rmSync(d, { recursive: true, force: true });
  });

  test.skipIf(process.platform === "win32")("resolves null on timeout instead of hanging", async () => {
    // A fake shell that ignores its args and blocks: the timeout must fire,
    // kill it, and yield null rather than wedging the daemon.
    const dir = mkdtempSync(join(tmpdir(), "autosk-shell-"));
    tmpDirs.push(dir);
    const fake = join(dir, "slowsh");
    writeFileSync(fake, "#!/bin/sh\nsleep 30\n");
    chmodSync(fake, 0o755);
    const started = Date.now();
    const result = await queryLoginShellPath({ shell: fake, timeoutMs: 100 });
    expect(result).toBeNull();
    expect(Date.now() - started).toBeLessThan(5000);
  });

  test.skipIf(process.platform === "win32")("resolves null when the shell binary does not exist", async () => {
    const result = await queryLoginShellPath({ shell: "/nonexistent/autosk-no-such-shell" });
    expect(result).toBeNull();
  });
});
