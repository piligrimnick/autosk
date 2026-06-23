import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  UI_SCALE_DEFAULT,
  UI_SCALE_MAX,
  UI_SCALE_MIN,
  clampUiScale,
  formatUiScale,
  loadUiScale,
  saveUiScale,
} from "./uiScale";

describe("clampUiScale", () => {
  it("snaps to the nearest 0.1 step", () => {
    expect(clampUiScale(1.04)).toBe(1);
    expect(clampUiScale(1.06)).toBe(1.1);
  });

  it("clamps into [MIN, MAX]", () => {
    expect(clampUiScale(0.1)).toBe(UI_SCALE_MIN);
    expect(clampUiScale(99)).toBe(UI_SCALE_MAX);
  });

  it("falls back to the default for non-finite input", () => {
    expect(clampUiScale(Number.NaN)).toBe(UI_SCALE_DEFAULT);
    expect(clampUiScale(Number.POSITIVE_INFINITY)).toBe(UI_SCALE_DEFAULT);
  });
});

describe("formatUiScale", () => {
  it("renders a rounded percentage", () => {
    expect(formatUiScale(1)).toBe("100%");
    expect(formatUiScale(1.1)).toBe("110%");
    expect(formatUiScale(0.5)).toBe("50%");
  });
});

describe("loadUiScale / saveUiScale", () => {
  // The util is browser-only; under the node test env we stub a minimal
  // in-memory `window.localStorage` (mirroring how platform.test stubs
  // `navigator`).
  let store: Map<string, string>;

  beforeEach(() => {
    store = new Map();
    vi.stubGlobal("window", {
      localStorage: {
        getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
        setItem: (k: string, v: string) => void store.set(k, v),
      },
    });
  });
  afterEach(() => vi.unstubAllGlobals());

  it("defaults when nothing is persisted", () => {
    expect(loadUiScale()).toBe(UI_SCALE_DEFAULT);
  });

  it("round-trips a clamped value", () => {
    saveUiScale(1.3);
    expect(loadUiScale()).toBe(1.3);
  });

  it("clamps a corrupt stored value on read", () => {
    store.set("autosk.uiScale", "nonsense");
    expect(loadUiScale()).toBe(UI_SCALE_DEFAULT);
  });
});
