/**
 * Unit tests for the free-form `metadata` helpers (plan §3): the reserved
 * `step_visits` counter accessors and the dot-path set/unset merge primitives
 * that back the `task.metadata.set` / `task.metadata.unset` RPCs.
 */

import { describe, expect, test } from "bun:test";

import {
  STEP_VISITS_KEY,
  applyMetadataPatch,
  applyMetadataUnset,
  bumpStepVisit,
  getStepVisits,
  isEmptyMetadata,
} from "../src/index.ts";

describe("isEmptyMetadata", () => {
  test("treats null/undefined/{} as empty, a populated bag as non-empty", () => {
    expect(isEmptyMetadata(undefined)).toBe(true);
    expect(isEmptyMetadata(null)).toBe(true);
    expect(isEmptyMetadata({})).toBe(true);
    expect(isEmptyMetadata({ a: 1 })).toBe(false);
    expect(isEmptyMetadata({ step_visits: {} })).toBe(false);
  });
});

describe("getStepVisits — defensive read", () => {
  test("returns {} when step_visits is absent or not an object", () => {
    expect(getStepVisits(undefined)).toEqual({});
    expect(getStepVisits({})).toEqual({});
    expect(getStepVisits({ step_visits: "nope" })).toEqual({});
    expect(getStepVisits({ step_visits: [1, 2] })).toEqual({});
    expect(getStepVisits({ step_visits: null })).toEqual({});
  });

  test("keeps only finite numeric entries (tolerating the float64 wire shape)", () => {
    const meta = {
      step_visits: { dev: 3, review: 1, bad: "x", nan: Number.NaN, inf: Infinity, float: 2 },
    };
    expect(getStepVisits(meta)).toEqual({ dev: 3, review: 1, float: 2 });
  });

  test("returns a fresh object that does not alias the stored bag", () => {
    const meta = { step_visits: { dev: 1 } };
    const out = getStepVisits(meta);
    out.dev = 99;
    expect((meta.step_visits as Record<string, number>).dev).toBe(1);
  });
});

describe("bumpStepVisit", () => {
  test("creates the reserved sub-object + entry on first bump", () => {
    const meta: Record<string, unknown> = {};
    bumpStepVisit(meta, "dev");
    expect(meta[STEP_VISITS_KEY]).toEqual({ dev: 1 });
  });

  test("increments an existing entry and leaves siblings untouched", () => {
    const meta: Record<string, unknown> = { step_visits: { dev: 2, review: 1 } };
    bumpStepVisit(meta, "dev");
    expect(meta.step_visits).toEqual({ dev: 3, review: 1 });
  });

  test("self-heals a corrupt step_visits to a clean count", () => {
    const meta: Record<string, unknown> = { step_visits: "broken" };
    bumpStepVisit(meta, "dev");
    expect(meta.step_visits).toEqual({ dev: 1 });
  });
});

describe("applyMetadataPatch — dot-path set", () => {
  test("sets a top-level leaf", () => {
    const meta: Record<string, unknown> = {};
    applyMetadataPatch(meta, { note: "hello" });
    expect(meta).toEqual({ note: "hello" });
  });

  test("creates intermediate objects for a nested dot-path", () => {
    const meta: Record<string, unknown> = {};
    applyMetadataPatch(meta, { "step_visits.dev": 0 });
    expect(meta).toEqual({ step_visits: { dev: 0 } });
  });

  test("merges into an existing sub-object without dropping siblings", () => {
    const meta: Record<string, unknown> = { step_visits: { dev: 5 } };
    applyMetadataPatch(meta, { "step_visits.review": 2 });
    expect(meta).toEqual({ step_visits: { dev: 5, review: 2 } });
  });

  test("overwrites a non-object segment so a deeper write can land", () => {
    const meta: Record<string, unknown> = { a: 7 };
    applyMetadataPatch(meta, { "a.b": 1 });
    expect(meta).toEqual({ a: { b: 1 } });
  });

  test("applies multiple entries in one patch", () => {
    const meta: Record<string, unknown> = {};
    applyMetadataPatch(meta, { "step_visits.dev": 1, note: { kept: true } });
    expect(meta).toEqual({ step_visits: { dev: 1 }, note: { kept: true } });
  });
});

describe("applyMetadataUnset — dot-path delete + prune", () => {
  test("removes a top-level key", () => {
    const meta: Record<string, unknown> = { a: 1, b: 2 };
    applyMetadataUnset(meta, ["a"]);
    expect(meta).toEqual({ b: 2 });
  });

  test("removes a nested leaf and prunes the emptied parent", () => {
    const meta: Record<string, unknown> = { step_visits: { dev: 1 } };
    applyMetadataUnset(meta, ["step_visits.dev"]);
    expect(meta).toEqual({});
  });

  test("keeps a parent that still has siblings after the delete", () => {
    const meta: Record<string, unknown> = { step_visits: { dev: 1, review: 2 } };
    applyMetadataUnset(meta, ["step_visits.dev"]);
    expect(meta).toEqual({ step_visits: { review: 2 } });
  });

  test("unsetting the whole reserved key drops every counter", () => {
    const meta: Record<string, unknown> = { step_visits: { dev: 3, review: 1 }, note: "x" };
    applyMetadataUnset(meta, ["step_visits"]);
    expect(meta).toEqual({ note: "x" });
  });

  test("an unknown path is a no-op", () => {
    const meta: Record<string, unknown> = { a: 1 };
    applyMetadataUnset(meta, ["nope", "a.b.c"]);
    expect(meta).toEqual({ a: 1 });
  });
});
