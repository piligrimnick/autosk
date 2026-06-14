/**
 * settings.json writers: upsert (dedup by identity / npm version replace) +
 * remove (match by name/path, any version), and the `{"extensions":[...]}` +
 * trailing-newline format. The parent dir is created on demand.
 */

import { afterEach, describe, expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import {
  readSettingsExtensions,
  removeExtensionEntry,
  upsertExtensionEntry,
  type ExtensionSource,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

const npm = (spec: string, name: string): ExtensionSource => ({ kind: "npm", spec, name });
const local = (path: string): ExtensionSource => ({ kind: "local", path });

describe("upsertExtensionEntry", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  test("creates the file (+ parent dir) and writes npm:<spec> with a trailing newline", () => {
    const dir = tempDir();
    cleanups.push(dir.cleanup);
    const settingsPath = join(dir.path, ".autosk", "settings.json");

    const res = upsertExtensionEntry(settingsPath, npm("@scope/pkg@1.0.0", "@scope/pkg"));
    expect(res).toEqual({ entry: "npm:@scope/pkg@1.0.0", changed: true });
    expect(existsSync(settingsPath)).toBe(true);
    const text = readFileSync(settingsPath, "utf8");
    expect(text.endsWith("\n")).toBe(true);
    expect(JSON.parse(text)).toEqual({ extensions: ["npm:@scope/pkg@1.0.0"] });
  });

  test("a re-install with a different version REPLACES the pin (dedup by name)", () => {
    const dir = tempDir();
    cleanups.push(dir.cleanup);
    const settingsPath = join(dir.path, ".autosk", "settings.json");

    upsertExtensionEntry(settingsPath, npm("@scope/pkg@1.0.0", "@scope/pkg"));
    const res = upsertExtensionEntry(settingsPath, npm("@scope/pkg@2.0.0", "@scope/pkg"));
    expect(res.changed).toBe(true);
    expect(readSettingsExtensions(settingsPath)).toEqual(["npm:@scope/pkg@2.0.0"]);
  });

  test("re-upserting the identical entry is a no-op (changed:false)", () => {
    const dir = tempDir();
    cleanups.push(dir.cleanup);
    const settingsPath = join(dir.path, ".autosk", "settings.json");

    upsertExtensionEntry(settingsPath, npm("@scope/pkg", "@scope/pkg"));
    const res = upsertExtensionEntry(settingsPath, npm("@scope/pkg", "@scope/pkg"));
    expect(res.changed).toBe(false);
    expect(readSettingsExtensions(settingsPath)).toEqual(["npm:@scope/pkg"]);
  });

  test("preserves unrelated entries and appends the new one", () => {
    const dir = tempDir();
    cleanups.push(dir.cleanup);
    const settingsPath = join(dir.path, ".autosk", "settings.json");

    upsertExtensionEntry(settingsPath, npm("@a/one", "@a/one"));
    upsertExtensionEntry(settingsPath, local("/opt/ext"));
    upsertExtensionEntry(settingsPath, npm("@b/two", "@b/two"));
    expect(readSettingsExtensions(settingsPath)).toEqual(["npm:@a/one", "/opt/ext", "npm:@b/two"]);
  });

  test("a local path dedups by absolute path", () => {
    const dir = tempDir();
    cleanups.push(dir.cleanup);
    const settingsPath = join(dir.path, ".autosk", "settings.json");

    upsertExtensionEntry(settingsPath, local("/opt/ext"));
    const res = upsertExtensionEntry(settingsPath, local("/opt/ext"));
    expect(res.changed).toBe(false);
    expect(readSettingsExtensions(settingsPath)).toEqual(["/opt/ext"]);
  });
});

describe("removeExtensionEntry", () => {
  const cleanups: (() => void)[] = [];
  afterEach(() => {
    for (const c of cleanups.splice(0)) c();
  });

  test("removes a matching npm entry by name (any version) and rewrites the file", () => {
    const dir = tempDir();
    cleanups.push(dir.cleanup);
    const settingsPath = join(dir.path, ".autosk", "settings.json");
    upsertExtensionEntry(settingsPath, npm("@scope/pkg@1.0.0", "@scope/pkg"));
    upsertExtensionEntry(settingsPath, npm("@other/keep", "@other/keep"));

    const res = removeExtensionEntry(settingsPath, npm("@scope/pkg", "@scope/pkg"));
    expect(res.removed).toEqual(["npm:@scope/pkg@1.0.0"]);
    expect(readSettingsExtensions(settingsPath)).toEqual(["npm:@other/keep"]);
  });

  test("removes a matching local entry by path", () => {
    const dir = tempDir();
    cleanups.push(dir.cleanup);
    const settingsPath = join(dir.path, ".autosk", "settings.json");
    upsertExtensionEntry(settingsPath, local("/opt/ext"));

    const res = removeExtensionEntry(settingsPath, local("/opt/ext"));
    expect(res.removed).toEqual(["/opt/ext"]);
    expect(readSettingsExtensions(settingsPath)).toEqual([]);
  });

  test("removing a non-present source is a no-op (no file created for a missing file)", () => {
    const dir = tempDir();
    cleanups.push(dir.cleanup);
    const settingsPath = join(dir.path, ".autosk", "settings.json");

    const res = removeExtensionEntry(settingsPath, npm("@scope/none", "@scope/none"));
    expect(res.removed).toEqual([]);
    expect(existsSync(settingsPath)).toBe(false);
  });
});
