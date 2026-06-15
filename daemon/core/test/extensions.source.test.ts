/**
 * Extension source parsing + classification (the `autosk ext` model).
 *
 * Covers: npm: vs local-path detection, `~` expansion, relative resolution
 * against cwd (install args) vs baseDir (settings entries), scoped-name version
 * stripping, the explicit-source rule (a bare token is rejected on install and
 * diagnosed in settings), and the settings round-trip helpers.
 */

import { describe, expect, test } from "bun:test";
import { isAbsolute, resolve } from "node:path";

import {
  InvalidExtensionSourceError,
  classifySettingsEntry,
  npmName,
  parseInstallSource,
  sameSource,
  settingsEntryFor,
  type ExtensionSource,
} from "../src/index.ts";

const HOME = "/home/u";
const CWD = "/work/proj";
const BASE = "/work/proj/.autosk";

describe("npmName — scoped + unscoped version stripping", () => {
  test.each([
    ["@scope/pkg", "@scope/pkg"],
    ["@scope/pkg@1.2.3", "@scope/pkg"],
    ["@scope/pkg@^1.0.0", "@scope/pkg"],
    ["@scope/pkg@latest", "@scope/pkg"],
    ["pkg", "pkg"],
    ["pkg@1.2.3", "pkg"],
    ["@autosk/feature-dev", "@autosk/feature-dev"],
  ])("npmName(%p) === %p", (spec, name) => {
    expect(npmName(spec)).toBe(name);
  });
});

describe("parseInstallSource — install argument", () => {
  test("npm: spec → npm source carrying spec + name", () => {
    expect(parseInstallSource("npm:@scope/pkg@1.2.3", { cwd: CWD, home: HOME })).toEqual({
      kind: "npm",
      spec: "@scope/pkg@1.2.3",
      name: "@scope/pkg",
    });
  });

  test("npm: without a version", () => {
    expect(parseInstallSource("npm:@autosk/feature-dev", { cwd: CWD, home: HOME })).toEqual({
      kind: "npm",
      spec: "@autosk/feature-dev",
      name: "@autosk/feature-dev",
    });
  });

  test("a relative path resolves against cwd", () => {
    expect(parseInstallSource("./my-ext", { cwd: CWD, home: HOME })).toEqual({
      kind: "local",
      path: resolve(CWD, "my-ext"),
    });
    expect(parseInstallSource("../sibling", { cwd: CWD, home: HOME })).toEqual({
      kind: "local",
      path: resolve(CWD, "../sibling"),
    });
  });

  test("an absolute path stays absolute", () => {
    expect(parseInstallSource("/opt/ext", { cwd: CWD, home: HOME })).toEqual({ kind: "local", path: "/opt/ext" });
  });

  test("a ~ path expands against home", () => {
    expect(parseInstallSource("~/ext", { cwd: CWD, home: HOME })).toEqual({ kind: "local", path: `${HOME}/ext` });
    expect(parseInstallSource("~", { cwd: CWD, home: HOME })).toEqual({ kind: "local", path: HOME });
  });

  test("a bare name is rejected (no implicit bare-name → npm)", () => {
    expect(() => parseInstallSource("my-ext", { cwd: CWD, home: HOME })).toThrow(InvalidExtensionSourceError);
    expect(() => parseInstallSource("@scope/pkg", { cwd: CWD, home: HOME })).toThrow(/unrecognised extension source/);
  });

  test("an empty / npm-without-spec source is rejected", () => {
    expect(() => parseInstallSource("   ", { cwd: CWD, home: HOME })).toThrow(InvalidExtensionSourceError);
    expect(() => parseInstallSource("npm:", { cwd: CWD, home: HOME })).toThrow(/missing package spec/);
  });
});

describe("classifySettingsEntry — settings.json entry", () => {
  test("npm: spec → npm source", () => {
    expect(classifySettingsEntry("npm:@scope/pkg@2.0.0", { baseDir: BASE, home: HOME })).toEqual({
      kind: "npm",
      spec: "@scope/pkg@2.0.0",
      name: "@scope/pkg",
    });
  });

  test("an absolute path → local source", () => {
    expect(classifySettingsEntry("/opt/ext", { baseDir: BASE, home: HOME })).toEqual({ kind: "local", path: "/opt/ext" });
  });

  test("a relative path resolves against baseDir (NOT cwd)", () => {
    expect(classifySettingsEntry("./rel", { baseDir: BASE, home: HOME })).toEqual({
      kind: "local",
      path: resolve(BASE, "rel"),
    });
  });

  test("a bare token → invalid (diagnostic, never a throw)", () => {
    const c = classifySettingsEntry("bare-name", { baseDir: BASE, home: HOME });
    expect(c.kind).toBe("invalid");
    if (c.kind === "invalid") expect(c.reason).toMatch(/unrecognised extension entry/);
  });

  test("an empty entry → invalid", () => {
    expect(classifySettingsEntry("   ", { baseDir: BASE, home: HOME }).kind).toBe("invalid");
  });
});

describe("settingsEntryFor + sameSource — round-trip + identity", () => {
  test("settingsEntryFor renders npm:<spec> and the absolute path", () => {
    expect(settingsEntryFor({ kind: "npm", spec: "@scope/pkg@1.0.0", name: "@scope/pkg" })).toBe("npm:@scope/pkg@1.0.0");
    expect(settingsEntryFor({ kind: "local", path: "/opt/ext" })).toBe("/opt/ext");
  });

  test("npm identity is by name (version ignored)", () => {
    const a: ExtensionSource = { kind: "npm", spec: "@s/p@1.0.0", name: "@s/p" };
    const b: ExtensionSource = { kind: "npm", spec: "@s/p@2.0.0", name: "@s/p" };
    expect(sameSource(a, b)).toBe(true);
    expect(sameSource(a, { kind: "npm", spec: "@s/other", name: "@s/other" })).toBe(false);
  });

  test("local identity is by path; cross-kind never matches", () => {
    expect(sameSource({ kind: "local", path: "/a" }, { kind: "local", path: "/a" })).toBe(true);
    expect(sameSource({ kind: "local", path: "/a" }, { kind: "local", path: "/b" })).toBe(false);
    expect(sameSource({ kind: "local", path: "/a" }, { kind: "npm", spec: "x", name: "x" })).toBe(false);
  });

  test("a settings entry round-trips through classify → settingsEntryFor", () => {
    const c = classifySettingsEntry("npm:@s/p@3", { baseDir: BASE, home: HOME });
    if (c.kind === "invalid") throw new Error("unexpected invalid");
    expect(settingsEntryFor(c)).toBe("npm:@s/p@3");
    // And local round-trips to its absolute path.
    const abs = "/opt/ext";
    const lc = classifySettingsEntry(abs, { baseDir: BASE, home: HOME });
    if (lc.kind === "invalid") throw new Error("unexpected invalid");
    expect(isAbsolute(settingsEntryFor(lc))).toBe(true);
  });
});
