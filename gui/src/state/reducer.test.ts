// Unit tests for the pure slice reducer (no browser, no daemon). These cover
// the correctness-critical logic the whole transcript + sidebar depend on:
//   * timelineSlice replay-then-tail dedup (the anti-duplication invariant);
//   * projectsSlice projects/loaded pruning + slice-retention + auto-select.

import { describe, it, expect } from "vitest";
import { rootReducer } from "./reducer";
import { emptyProjectSlice, initialState, type AppState } from "./types";
import type { MessageEvent, ProjectInfo } from "@/types";

function msg(text: string): MessageEvent {
  return { kind: "assistant_text", text };
}

function project(root: string): ProjectInfo {
  return { root, db_path: `${root}/.autosk/db`, name: root.split("/").pop() ?? root };
}

describe("timelineSlice", () => {
  it("messagesReset seeds the transcript and clears the seen counter", () => {
    let s = initialState();
    // Pre-load a stale seen counter so we can prove the reset clears it.
    s = rootReducer(s, { type: "job/messageAppended", jobId: "j1", eventId: 5, message: msg("old") });
    expect(s.seenEventId["j1"]).toBe(5);

    s = rootReducer(s, { type: "job/messagesReset", jobId: "j1", messages: [msg("a"), msg("b")] });
    expect(s.messagesByJob["j1"]).toHaveLength(2);
    expect(s.seenEventId["j1"]).toBe(0);
  });

  it("messageAppended dedups replayed frames (eventId <= seen) and tracks the max", () => {
    let s: AppState = initialState();
    for (const id of [1, 2, 3]) {
      s = rootReducer(s, { type: "job/messageAppended", jobId: "j1", eventId: id, message: msg(`#${id}`) });
    }
    expect(s.messagesByJob["j1"]).toHaveLength(3);
    expect(s.seenEventId["j1"]).toBe(3);

    // A replayed frame (id 2 <= seen 3) is dropped, leaving state untouched.
    const before = s;
    s = rootReducer(s, { type: "job/messageAppended", jobId: "j1", eventId: 2, message: msg("dup") });
    expect(s).toBe(before);
    expect(s.messagesByJob["j1"]).toHaveLength(3);
  });

  it("messageAppended with eventId 0 (live frame, no id) always appends", () => {
    let s = initialState();
    s = rootReducer(s, { type: "job/messageAppended", jobId: "j1", eventId: 3, message: msg("#3") });
    s = rootReducer(s, { type: "job/messageAppended", jobId: "j1", eventId: 0, message: msg("live") });
    s = rootReducer(s, { type: "job/messageAppended", jobId: "j1", eventId: 0, message: msg("live2") });
    expect(s.messagesByJob["j1"]).toHaveLength(3);
    // seen stays at the highest real id seen.
    expect(s.seenEventId["j1"]).toBe(3);
  });
});

describe("projectsSlice projects/loaded", () => {
  it("prunes slices for dropped projects, keeps the rest, auto-selects the first", () => {
    let s = initialState();
    s = { ...s, byProject: { "/a": emptyProjectSlice(), "/b": emptyProjectSlice() } };

    s = rootReducer(s, { type: "projects/loaded", projects: [project("/b"), project("/c")] });

    expect(s.projectsLoaded).toBe(true);
    expect(Object.keys(s.byProject).sort()).toEqual(["/b"]); // /a pruned, /c has no slice yet
    expect(s.activeProject).toBe("/b"); // auto-select first when none active
  });

  it("keeps the active project when it is still registered", () => {
    let s = initialState();
    s = { ...s, activeProject: "/a", byProject: { "/a": emptyProjectSlice() } };

    s = rootReducer(s, { type: "projects/loaded", projects: [project("/a"), project("/b")] });
    expect(s.activeProject).toBe("/a");
  });

  it("re-selects the first project when the active one disappears", () => {
    let s = initialState();
    s = { ...s, activeProject: "/a", byProject: { "/a": emptyProjectSlice() } };

    s = rootReducer(s, { type: "projects/loaded", projects: [project("/b"), project("/c")] });
    expect(s.activeProject).toBe("/b");
  });

  it("leaves activeProject null when the registry is empty", () => {
    let s = initialState();
    s = rootReducer(s, { type: "projects/loaded", projects: [] });
    expect(s.activeProject).toBeNull();
    expect(s.projectsLoaded).toBe(true);
  });
});
