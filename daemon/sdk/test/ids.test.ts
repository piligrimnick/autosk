import { describe, expect, test } from "bun:test";
import { newEntryId, newSessionId, newTaskId } from "../src/index.ts";

describe("newTaskId", () => {
  test("matches ask-<6 hex>", () => {
    for (let i = 0; i < 100; i++) {
      expect(newTaskId()).toMatch(/^ask-[0-9a-f]{6}$/);
    }
  });

  test("is unique across many draws", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 1000; i++) seen.add(newTaskId());
    // 24 bits of entropy — collisions in 1000 draws are vanishingly unlikely.
    expect(seen.size).toBeGreaterThan(990);
  });
});

describe("newSessionId", () => {
  test("is a valid UUIDv7 string", () => {
    const id = newSessionId();
    expect(id).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    );
  });

  test("is time-ordered: later ids sort after earlier ones", async () => {
    const a = newSessionId();
    await new Promise((r) => setTimeout(r, 5));
    const b = newSessionId();
    expect(a < b).toBe(true);
  });
});

describe("newEntryId", () => {
  test("is 8 hex chars", () => {
    expect(newEntryId()).toMatch(/^[0-9a-f]{8}$/);
  });

  test("avoids ids in the taken set", () => {
    const taken = new Set<string>();
    for (let i = 0; i < 500; i++) {
      const id = newEntryId(taken);
      expect(taken.has(id)).toBe(false);
      taken.add(id);
    }
    expect(taken.size).toBe(500);
  });

  test("accepts a predicate as the taken check", () => {
    const id = newEntryId((candidate) => candidate === "deadbeef");
    expect(id).not.toBe("deadbeef");
  });
});
