// Unit tests for `hydratedInitialState()` (ask-8c2aee): the store must seed
// `activeProject` from the persisted value BEFORE `projects/loaded` runs, so
// the reducer's existing validation/fallback (reducer.test.ts) sees it.
//
// Mirrors uiScale.test.ts / the sidebar-geometry hydration: stub a minimal
// in-memory `window.localStorage` under vitest's node environment.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { hydratedInitialState } from "./store";
import { initialState } from "./types";
import { ACTIVE_PROJECT_STORAGE_KEY } from "@/features/projects/utils/activeProject";

describe("hydratedInitialState", () => {
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

  it("seeds activeProject with the persisted root when one is present", () => {
    store.set(ACTIVE_PROJECT_STORAGE_KEY, "/home/me/projB");
    const state = hydratedInitialState();
    expect(state.activeProject).toBe("/home/me/projB");
  });

  it("yields activeProject: null when no project is persisted", () => {
    const state = hydratedInitialState();
    expect(state.activeProject).toBeNull();
  });

  it("does not disturb the rest of the default state when hydrating activeProject", () => {
    store.set(ACTIVE_PROJECT_STORAGE_KEY, "/home/me/projB");
    const state = hydratedInitialState();
    const base = initialState();
    expect(state.projects).toEqual(base.projects);
    expect(state.projectsLoaded).toBe(base.projectsLoaded);
    expect(state.selection).toEqual(base.selection);
  });
});

describe("hydratedInitialState outside the browser", () => {
  it("yields activeProject: null and never throws when window is undefined", () => {
    // vitest's node test environment has no `window` by default.
    expect(typeof window).toBe("undefined");
    expect(() => hydratedInitialState()).not.toThrow();
    expect(hydratedInitialState().activeProject).toBeNull();
  });
});
