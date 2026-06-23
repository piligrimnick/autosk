// Unit tests for the extension-browser pure helpers: the `source` → package
// name parser (scoped + versioned), the installed-scope mapping (project wins
// over global), and the small formatters. Pure; no daemon, no Tauri bridge.

import { describe, it, expect } from "vitest";
import type { ExtensionEntryInfo } from "@/types";
import { formatDownloads, installedNameFromSource, installedScopes, npmName } from "./extensions";

describe("npmName", () => {
  it("strips a trailing @version from an unscoped spec", () => {
    expect(npmName("pkg@1.2.3")).toBe("pkg");
  });
  it("strips a trailing @version from a scoped spec", () => {
    expect(npmName("@autosk/feature-dev@0.1.2")).toBe("@autosk/feature-dev");
  });
  it("leaves a scoped spec without a version untouched", () => {
    expect(npmName("@autosk/worktree")).toBe("@autosk/worktree");
  });
  it("leaves an unscoped spec without a version untouched", () => {
    expect(npmName("plain-pkg")).toBe("plain-pkg");
  });
  it("does not treat a leading scope @ as a version separator", () => {
    expect(npmName("@scope/name")).toBe("@scope/name");
  });
});

describe("installedNameFromSource", () => {
  it("parses an npm source", () => {
    expect(installedNameFromSource("npm:@autosk/feature-dev@0.1.2")).toBe("@autosk/feature-dev");
    expect(installedNameFromSource("npm:left-pad@1.3.0")).toBe("left-pad");
  });
  it("ignores local-path and invalid entries", () => {
    expect(installedNameFromSource("/abs/path/to/ext")).toBeNull();
    expect(installedNameFromSource("./rel")).toBeNull();
    expect(installedNameFromSource("garbage")).toBeNull();
    expect(installedNameFromSource("npm:")).toBeNull();
  });
});

describe("installedScopes", () => {
  const entry = (source: string, scope: "global" | "project"): ExtensionEntryInfo => ({
    source,
    scope,
    kind: "npm",
    resolved: true,
  });

  it("maps each npm name to its scope", () => {
    const m = installedScopes([
      entry("npm:@autosk/feature-dev@0.1.2", "global"),
      entry("npm:local-only@1.0.0", "project"),
    ]);
    expect(m.get("@autosk/feature-dev")).toBe("global");
    expect(m.get("local-only")).toBe("project");
    expect(m.size).toBe(2);
  });

  it("prefers project scope when a package is installed in both", () => {
    const m = installedScopes([
      entry("npm:dup@1.0.0", "global"),
      entry("npm:dup@2.0.0", "project"),
    ]);
    expect(m.get("dup")).toBe("project");
  });

  it("skips non-npm entries", () => {
    const m = installedScopes([entry("/abs/local", "project"), { source: "junk", scope: "global", kind: "invalid", resolved: false }]);
    expect(m.size).toBe(0);
  });
});

describe("formatDownloads", () => {
  it("groups thousands", () => {
    expect(formatDownloads(0)).toBe("0");
    expect(formatDownloads(42)).toBe("42");
    expect(formatDownloads(1234)).toBe((1234).toLocaleString());
  });
  it("clamps invalid input to 0", () => {
    expect(formatDownloads(-5)).toBe("0");
    expect(formatDownloads(Number.NaN)).toBe("0");
  });
});
