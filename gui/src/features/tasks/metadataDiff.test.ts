import { describe, expect, it } from "vitest";

import { computeMetadataDiff, metadataToText } from "./metadataDiff";

describe("metadataToText", () => {
  it("renders an empty bag as {}", () => {
    expect(metadataToText({})).toBe("{}");
    expect(metadataToText(undefined)).toBe("{}");
  });
  it("pretty-prints a populated bag", () => {
    expect(metadataToText({ a: 1 })).toBe('{\n  "a": 1\n}');
  });
});

describe("computeMetadataDiff", () => {
  const old = {
    step_visits: { dev: 2, review: 1 },
    note: "keep",
    drop: true,
  };

  it("returns empty patch + unset for an unchanged document", () => {
    const text = JSON.stringify(old);
    expect(computeMetadataDiff(old, text)).toEqual({ patch: {}, unset: [] });
  });

  it("sends only changed/added top-level keys as the patch", () => {
    const text = JSON.stringify({
      step_visits: { dev: 5, review: 1 },
      note: "keep",
      added: 7,
    });
    expect(computeMetadataDiff(old, text)).toEqual({
      patch: { step_visits: { dev: 5, review: 1 }, added: 7 },
      unset: ["drop"],
    });
  });

  it("lists removed top-level keys in the unset list", () => {
    const text = JSON.stringify({ note: "keep" });
    expect(computeMetadataDiff(old, text)).toEqual({
      patch: {},
      unset: ["step_visits", "drop"],
    });
  });

  it("treats an empty/whitespace document as clear-all", () => {
    expect(computeMetadataDiff(old, "   ")).toEqual({
      patch: {},
      unset: ["step_visits", "note", "drop"],
    });
  });

  it("throws on invalid JSON", () => {
    expect(() => computeMetadataDiff(old, "{not json")).toThrow();
  });

  it("throws on a non-object document", () => {
    expect(() => computeMetadataDiff(old, "[1,2,3]")).toThrow(/must be a JSON object/);
  });
});
