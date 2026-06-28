import { describe, expect, it } from "vitest";

import { formatterFor } from "./toolFormatters";

const summary = (name: string, args: Record<string, unknown>) =>
  formatterFor(name).summary(args);

describe("formatterFor", () => {
  it("is case-insensitive", () => {
    expect(summary("READ", { path: "/a" })).toEqual({ primary: "/a", hint: undefined });
  });

  it("falls back to an empty summary for unknown tools", () => {
    expect(summary("totally_unknown", { path: "/a", foo: 1 })).toEqual({});
  });
});

describe("per-tool summaries", () => {
  it("read: path + offset/limit hints", () => {
    expect(summary("read", { path: "/etc/hosts" })).toEqual({
      primary: "/etc/hosts",
      hint: undefined,
    });
    expect(summary("read", { path: "/f", offset: 5, limit: 20 })).toEqual({
      primary: "/f",
      hint: "from line 5 (20 lines)",
    });
    expect(summary("read", { path: "/f", limit: 20 })).toEqual({
      primary: "/f",
      hint: "(20 lines)",
    });
  });

  it("bash: first line of the command", () => {
    expect(summary("bash", { command: "ls -la" })).toEqual({ primary: "ls -la" });
    expect(summary("bash", { command: "echo one\necho two" })).toEqual({ primary: "echo one" });
    expect(summary("bash", {})).toEqual({ primary: undefined });
  });

  it("grep: quoted pattern + in path / glob", () => {
    expect(summary("grep", { pattern: "TODO" })).toEqual({
      primary: '"TODO"',
      hint: undefined,
    });
    expect(summary("grep", { pattern: "foo", path: "src", glob: "*.ts" })).toEqual({
      primary: '"foo"',
      hint: "in src (*.ts)",
    });
  });

  it("find: pattern + in path", () => {
    expect(summary("find", { pattern: "**/*.ts", path: "src" })).toEqual({
      primary: "**/*.ts",
      hint: "in src",
    });
    expect(summary("find", { pattern: "*.md" })).toEqual({ primary: "*.md", hint: undefined });
  });

  it("ls: path only", () => {
    expect(summary("ls", { path: "/tmp" })).toEqual({ primary: "/tmp" });
    expect(summary("ls", {})).toEqual({ primary: undefined });
  });

  it("write: path only", () => {
    expect(summary("write", { path: "/out.txt", content: "x" })).toEqual({ primary: "/out.txt" });
  });

  it("edit: path + edit count (singular/plural)", () => {
    expect(summary("edit", { path: "/f", edits: [{}] })).toEqual({
      primary: "/f",
      hint: "(1 edit)",
    });
    expect(summary("edit", { path: "/f", edits: [{}, {}, {}] })).toEqual({
      primary: "/f",
      hint: "(3 edits)",
    });
    expect(summary("edit", { path: "/f" })).toEqual({ primary: "/f", hint: undefined });
  });

  it("web_search: quoted query + domain/usage/country flags", () => {
    expect(summary("web_search", { query: "rust async" })).toEqual({
      primary: '"rust async"',
      hint: undefined,
    });
    expect(
      summary("web_search", {
        query: "q",
        allowed_domains: ["a.com", "b.com"],
        blocked_domains: ["c.com"],
        max_uses: 3,
        user_location: { type: "approximate", country: "US" },
      }),
    ).toEqual({ primary: '"q"', hint: "+2d -1d max=3 US" });
  });

  it("web_fetch: url or page count + prompt hint", () => {
    expect(summary("web_fetch", { url: "https://x.dev", prompt: "extract title" })).toEqual({
      primary: "https://x.dev",
      hint: "extract title",
    });
    expect(summary("web_fetch", { pages: [{}, {}] })).toEqual({
      primary: "2 pages",
      hint: undefined,
    });
    expect(summary("web_fetch", { pages: [{}] })).toEqual({ primary: "1 page", hint: undefined });
  });
});
