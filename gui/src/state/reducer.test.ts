// Unit tests for the pure slice reducer (no browser, no daemon). These cover
// the correctness-critical logic the whole transcript + sidebar depend on:
//   * transcriptSlice replay-then-tail dedup (the anti-duplication invariant);
//   * projectsSlice projects/loaded pruning + slice-retention + auto-select.

import { describe, it, expect } from "vitest";
import { rootReducer } from "./reducer";
import { emptyProjectSlice, initialState, type AppState } from "./types";
import type { ProjectInfo, SessionMeta, TranscriptLine } from "@/types";

function entry(id: string, text: string): TranscriptLine {
  return {
    type: "message",
    id,
    timestamp: "2024-01-01T00:00:00Z",
    message: { role: "user", content: text, timestamp: 0 },
  };
}

function project(root: string): ProjectInfo {
  return { root, name: root.split("/").pop() ?? root };
}

function sessionRow(
  id: string,
  startedAt: string | null,
  status: SessionMeta["status"] = "running",
): SessionMeta {
  return {
    id,
    kind: "task",
    task_id: "t1",
    workflow: "wf",
    step: "dev",
    agent: "a",
    status,
    started_at: startedAt,
    ended_at: null,
  };
}

describe("transcriptSlice", () => {
  it("transcriptReset seeds the transcript and sets the seen line to nextLine-1", () => {
    let s = initialState();
    // Pre-load a stale seen counter so we can prove the reset overrides it.
    s = rootReducer(s, { type: "session/transcriptAppended", sessionId: "s1", line: 5, entry: entry("e5", "old") });
    expect(s.seenLineBySession["s1"]).toBe(5);

    s = rootReducer(s, {
      type: "session/transcriptReset",
      sessionId: "s1",
      entries: [entry("a", "a"), entry("b", "b")],
      nextLine: 3,
    });
    expect(s.transcriptBySession["s1"]).toHaveLength(2);
    expect(s.seenLineBySession["s1"]).toBe(2); // covered up to nextLine - 1
  });

  it("transcriptAppended dedups replayed lines (line <= seen) and tracks the max", () => {
    let s: AppState = initialState();
    for (const id of [1, 2, 3]) {
      s = rootReducer(s, { type: "session/transcriptAppended", sessionId: "s1", line: id, entry: entry(`e${id}`, `#${id}`) });
    }
    expect(s.transcriptBySession["s1"]).toHaveLength(3);
    expect(s.seenLineBySession["s1"]).toBe(3);

    // A replayed line (2 <= seen 3) is dropped, leaving state untouched.
    const before = s;
    s = rootReducer(s, { type: "session/transcriptAppended", sessionId: "s1", line: 2, entry: entry("dup", "dup") });
    expect(s).toBe(before);
    expect(s.transcriptBySession["s1"]).toHaveLength(3);
  });

  it("transcriptAppended with line 0 (live frame, no cursor) always appends", () => {
    let s = initialState();
    s = rootReducer(s, { type: "session/transcriptAppended", sessionId: "s1", line: 3, entry: entry("e3", "#3") });
    s = rootReducer(s, { type: "session/transcriptAppended", sessionId: "s1", line: 0, entry: entry("l1", "live") });
    s = rootReducer(s, { type: "session/transcriptAppended", sessionId: "s1", line: 0, entry: entry("l2", "live2") });
    expect(s.transcriptBySession["s1"]).toHaveLength(3);
    // seen stays at the highest real line seen.
    expect(s.seenLineBySession["s1"]).toBe(3);
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

describe("selection + sessions slices", () => {
  it("project/select switches the project and clears the selection", () => {
    const s0: AppState = { ...initialState(), selection: { kind: "task", taskId: "t1" } };
    const s = rootReducer(s0, { type: "project/select", root: "/a" });
    expect(s.activeProject).toBe("/a");
    expect(s.selection).toEqual({ kind: "none" });
  });

  it("projects/loaded keeps the selection when the active project is unchanged", () => {
    const s0: AppState = {
      ...initialState(),
      activeProject: "/a",
      byProject: { "/a": emptyProjectSlice() },
      selection: { kind: "task", taskId: "t1" },
    };
    const s = rootReducer(s0, { type: "projects/loaded", projects: [project("/a")] });
    expect(s.activeProject).toBe("/a");
    expect(s.selection).toEqual({ kind: "task", taskId: "t1" });
  });

  it("sessions/loaded fills the session order newest-first and upserts sessions", () => {
    const a = sessionRow("a", "2024-01-01T00:00:00Z");
    const b = sessionRow("b", "2024-01-02T00:00:00Z");
    let s: AppState = { ...initialState(), activeProject: "/p" };
    s = rootReducer(s, { type: "sessions/loaded", root: "/p", sessions: [a, b] });
    expect(s.sessionOrderByProject["/p"]).toEqual(["b", "a"]); // newest first
    expect(s.sessions["a"]).toBeDefined();
    expect(s.sessions["b"]).toBeDefined();
  });

  it("sessions/loaded floats a queued (started_at:null) session to the top and breaks ties by id", () => {
    // A queued session (started_at:null) just spawned, so it counts as newest
    // and sorts first; two sessions sharing a started_at fall back to the id
    // tiebreak (descending) so the order is deterministic (a raw Date.parse
    // comparator would yield NaN and leave the order unstable). This locks the
    // null-safe key shared by the reducer and store.tsx's subscribeTaskLive.
    const queued = sessionRow("q", null, "queued");
    const m1 = sessionRow("m1", "2024-01-01T00:00:00Z");
    const m2 = sessionRow("m2", "2024-01-01T00:00:00Z");
    let s: AppState = { ...initialState(), activeProject: "/p" };
    s = rootReducer(s, { type: "sessions/loaded", root: "/p", sessions: [m1, queued, m2] });
    // queued (null → newest) first; the equal-timestamp pair tiebreaks by id
    // descending (m2 before m1), regardless of input order.
    expect(s.sessionOrderByProject["/p"]).toEqual(["q", "m2", "m1"]);
  });

  it("sidebar panel: ui/sidebarPanel sets the active accordion panel", () => {
    let s = initialState();
    expect(s.ui.sidebarPanel).toBe("tasks"); // default
    s = rootReducer(s, { type: "ui/sidebarPanel", panel: "workflows" });
    expect(s.ui.sidebarPanel).toBe("workflows");
  });

  it("sidebar panel: selecting an entity auto-expands the matching panel", () => {
    let s = initialState();
    s = rootReducer(s, { type: "selection/set", selection: { kind: "session", sessionId: "s1" } });
    expect(s.ui.sidebarPanel).toBe("sessions");
    s = rootReducer(s, { type: "selection/set", selection: { kind: "workflow", name: "wf" } });
    expect(s.ui.sidebarPanel).toBe("workflows");
    s = rootReducer(s, { type: "selection/set", selection: { kind: "task", taskId: "t1" } });
    expect(s.ui.sidebarPanel).toBe("tasks");
  });

  it("sidebar collapse: ui/sidebarToggle flips, ui/sidebarSetCollapsed sets", () => {
    let s = initialState();
    expect(s.ui.sidebarCollapsed).toBe(false); // default
    s = rootReducer(s, { type: "ui/sidebarToggle" });
    expect(s.ui.sidebarCollapsed).toBe(true);
    s = rootReducer(s, { type: "ui/sidebarToggle" });
    expect(s.ui.sidebarCollapsed).toBe(false);
    s = rootReducer(s, { type: "ui/sidebarSetCollapsed", collapsed: true });
    expect(s.ui.sidebarCollapsed).toBe(true);
  });

  it("sidebar width: ui/sidebarWidth clamps to [220, 480] and rounds", () => {
    let s = initialState();
    expect(s.ui.sidebarWidth).toBe(300); // default
    s = rootReducer(s, { type: "ui/sidebarWidth", width: 360.6 });
    expect(s.ui.sidebarWidth).toBe(361);
    s = rootReducer(s, { type: "ui/sidebarWidth", width: 50 });
    expect(s.ui.sidebarWidth).toBe(220); // floored to min
    s = rootReducer(s, { type: "ui/sidebarWidth", width: 9000 });
    expect(s.ui.sidebarWidth).toBe(480); // capped to max
  });

  it("ui/uiScale clamps to [0.5, 3] and snaps to the 0.1 step", () => {
    let s = initialState();
    expect(s.ui.uiScale).toBe(1); // default
    s = rootReducer(s, { type: "ui/uiScale", scale: 1.24 });
    expect(s.ui.uiScale).toBe(1.2); // snapped to nearest step
    s = rootReducer(s, { type: "ui/uiScale", scale: 0.1 });
    expect(s.ui.uiScale).toBe(0.5); // floored to min
    s = rootReducer(s, { type: "ui/uiScale", scale: 99 });
    expect(s.ui.uiScale).toBe(3); // capped to max
  });

  it("sidebar panel: clearing the selection leaves the active panel unchanged", () => {
    let s = rootReducer(initialState(), { type: "ui/sidebarPanel", panel: "workflows" });
    s = rootReducer(s, { type: "selection/set", selection: { kind: "none" } });
    expect(s.ui.sidebarPanel).toBe("workflows");
    expect(s.selection).toEqual({ kind: "none" });
  });

  it("sidebar panel: opening a modal does not disturb the active panel", () => {
    let s = rootReducer(initialState(), { type: "ui/sidebarPanel", panel: "sessions" });
    s = rootReducer(s, { type: "ui/modal", modal: "settings" });
    expect(s.ui.sidebarPanel).toBe("sessions");
    expect(s.ui.modal).toBe("settings");
  });

  it("session/upsert prepends a new session to the active project's session order, idempotently", () => {
    const a = sessionRow("a", "2024-01-01T00:00:00Z");
    let s: AppState = {
      ...initialState(),
      activeProject: "/p",
      sessions: { a },
      sessionOrderByProject: { "/p": ["a"] },
    };
    const c = sessionRow("c", "2024-01-03T00:00:00Z");
    s = rootReducer(s, { type: "session/upsert", session: c });
    expect(s.sessionOrderByProject["/p"]).toEqual(["c", "a"]);
    // Re-upserting an existing session updates the map but never duplicates the order.
    s = rootReducer(s, { type: "session/upsert", session: { ...c, status: "done" } });
    expect(s.sessionOrderByProject["/p"]).toEqual(["c", "a"]);
    expect(s.sessions["c"].status).toBe("done");
  });
});

describe("project/diagnosticsLoaded", () => {
  it("stores the project's extension load errors on its slice", () => {
    let s: AppState = { ...initialState(), activeProject: "/p" };
    s = rootReducer(s, {
      type: "project/diagnosticsLoaded",
      root: "/p",
      diagnostics: { root: "/p", extensions: [{ source: "bad-ext", error: "boom" }] },
    });
    expect(s.byProject["/p"].diagnostics?.extensions).toHaveLength(1);
    expect(s.byProject["/p"].diagnostics?.extensions[0].source).toBe("bad-ext");
  });
});
