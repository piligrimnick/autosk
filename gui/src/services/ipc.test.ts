// Unit tests for normalizeError — the funnel every daemon error passes through
// before the UI branches on `code`. The Rust backend surfaces daemon errors via
// Tauri's error channel as a JSON-encoded `{code,message,details}` string (and
// occasionally as a plain object/string), so the parser must cope with all
// shapes. Pure; no browser, no daemon.

import { describe, it, expect } from "vitest";
import { DaemonError, normalizeError } from "./ipc";

describe("normalizeError", () => {
  it("passes through an existing Error untouched", () => {
    const e = new Error("boom");
    expect(normalizeError(e)).toBe(e);
  });

  it("parses a JSON-encoded ErrorObject string into a DaemonError", () => {
    const out = normalizeError(JSON.stringify({ code: 1003, message: "not found", details: { id: "x" } }));
    expect(out).toBeInstanceOf(DaemonError);
    const de = out as DaemonError;
    expect(de.code).toBe(1003);
    expect(de.message).toBe("not found");
    expect(de.details).toEqual({ id: "x" });
  });

  it("maps a structured object with code+message to a DaemonError", () => {
    const out = normalizeError({ code: 1004, message: "conflict" });
    expect(out).toBeInstanceOf(DaemonError);
    expect((out as DaemonError).code).toBe(1004);
  });

  it("falls back to a plain Error for an object with only a message", () => {
    const out = normalizeError({ message: "plain" });
    expect(out).toBeInstanceOf(Error);
    expect(out).not.toBeInstanceOf(DaemonError);
    expect(out.message).toBe("plain");
  });

  it("wraps a non-JSON string as a plain Error", () => {
    const out = normalizeError("just a string");
    expect(out).toBeInstanceOf(Error);
    expect(out).not.toBeInstanceOf(DaemonError);
    expect(out.message).toBe("just a string");
  });

  it("stringifies anything else", () => {
    expect(normalizeError(42).message).toBe("42");
    expect(normalizeError(null).message).toBe("null");
  });
});
