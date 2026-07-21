// Unit tests for the active-project persistence helpers (ask-8c2aee).
//
// Mirrors uiScale.test.ts's approach: stub a minimal in-memory
// `window.localStorage` under vitest's node environment (no `window` by
// default), and additionally drive getItem/setItem/removeItem throwing to
// prove the helpers are non-throwing, best-effort no-ops in private-mode /
// quota-exceeded scenarios.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ACTIVE_PROJECT_STORAGE_KEY, loadActiveProject, saveActiveProject } from "./activeProject";

describe("ACTIVE_PROJECT_STORAGE_KEY", () => {
  it("is the contracted storage key", () => {
    expect(ACTIVE_PROJECT_STORAGE_KEY).toBe("autosk.activeProject");
  });
});

describe("loadActiveProject", () => {
  let store: Map<string, string>;

  beforeEach(() => {
    store = new Map();
    vi.stubGlobal("window", {
      localStorage: {
        getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
        setItem: (k: string, v: string) => void store.set(k, v),
        removeItem: (k: string) => void store.delete(k),
      },
    });
  });
  afterEach(() => vi.unstubAllGlobals());

  it("returns the persisted root string when present", () => {
    store.set(ACTIVE_PROJECT_STORAGE_KEY, "/home/me/projB");
    expect(loadActiveProject()).toBe("/home/me/projB");
  });

  it("returns null when the key is absent", () => {
    expect(loadActiveProject()).toBeNull();
  });

  it("returns null for an empty-string stored value (never returns \"\")", () => {
    store.set(ACTIVE_PROJECT_STORAGE_KEY, "");
    expect(loadActiveProject()).toBeNull();
  });

  it("returns null for a whitespace-only stored value", () => {
    store.set(ACTIVE_PROJECT_STORAGE_KEY, "   ");
    expect(loadActiveProject()).toBeNull();
  });

  it("returns null and does not throw when localStorage.getItem throws", () => {
    vi.stubGlobal("window", {
      localStorage: {
        getItem: () => {
          throw new Error("quota / private mode");
        },
      },
    });
    expect(() => loadActiveProject()).not.toThrow();
    expect(loadActiveProject()).toBeNull();
  });
});

describe("loadActiveProject outside the browser", () => {
  it("returns null and does not throw when window is undefined", () => {
    // vitest's node test environment has no `window` by default; no stub here
    // proves the non-browser path.
    expect(typeof window).toBe("undefined");
    expect(() => loadActiveProject()).not.toThrow();
    expect(loadActiveProject()).toBeNull();
  });
});

describe("saveActiveProject", () => {
  let store: Map<string, string>;

  beforeEach(() => {
    store = new Map();
    vi.stubGlobal("window", {
      localStorage: {
        getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
        setItem: (k: string, v: string) => void store.set(k, v),
        removeItem: (k: string) => void store.delete(k),
      },
    });
  });
  afterEach(() => vi.unstubAllGlobals());

  it("writes the given root string under the storage key", () => {
    saveActiveProject("/home/me/projA");
    expect(store.get(ACTIVE_PROJECT_STORAGE_KEY)).toBe("/home/me/projA");
  });

  it("round-trips through loadActiveProject", () => {
    saveActiveProject("/home/me/projA");
    expect(loadActiveProject()).toBe("/home/me/projA");
  });

  it("clears the persisted value when saving null", () => {
    saveActiveProject("/home/me/projA");
    saveActiveProject(null);
    expect(loadActiveProject()).toBeNull();
    expect(store.has(ACTIVE_PROJECT_STORAGE_KEY)).toBe(false);
  });

  it("clears the persisted value when saving an empty/whitespace-only string", () => {
    saveActiveProject("/home/me/projA");
    saveActiveProject("   ");
    expect(store.has(ACTIVE_PROJECT_STORAGE_KEY)).toBe(false);
  });

  it("does not throw when window is undefined", () => {
    vi.unstubAllGlobals();
    expect(typeof window).toBe("undefined");
    expect(() => saveActiveProject("/home/me/projA")).not.toThrow();
  });

  it("is a non-throwing no-op when localStorage.setItem throws", () => {
    vi.stubGlobal("window", {
      localStorage: {
        getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
        setItem: () => {
          throw new Error("quota exceeded");
        },
        removeItem: (k: string) => void store.delete(k),
      },
    });
    expect(() => saveActiveProject("/home/me/projA")).not.toThrow();
  });

  it("is a non-throwing no-op when localStorage.removeItem throws", () => {
    vi.stubGlobal("window", {
      localStorage: {
        getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
        setItem: (k: string, v: string) => void store.set(k, v),
        removeItem: () => {
          throw new Error("quota exceeded");
        },
      },
    });
    expect(() => saveActiveProject(null)).not.toThrow();
  });
});
