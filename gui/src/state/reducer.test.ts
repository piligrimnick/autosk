// Unit tests for the pure slice reducer (no browser, no daemon). These cover
// the correctness-critical logic the whole transcript + sidebar depend on:
//   * timelineSlice replay-then-tail dedup (the anti-duplication invariant);
//   * projectsSlice projects/loaded pruning + slice-retention + auto-select.

import { describe, it, expect } from "vitest";
import { rootReducer } from "./reducer";
import { emptyProjectSlice, initialState, type AppState } from "./types";
import type { Job, MessageEvent, ProjectInfo } from "@/types";

function msg(text: string): MessageEvent {
  return { kind: "assistant_text", text };
}

function project(root: string): ProjectInfo {
  return { root, db_path: `${root}/.autosk/db`, name: root.split("/").pop() ?? root };
}

function jobRow(id: string, createdAt: string): Job {
  return {
    job_id: id,
    task_id: "t1",
    step_id: "s1",
    status: "running",
    corrections_used: 0,
    max_corrections: 0,
    created_at: createdAt,
    duration_ms: 0,
    attach_count: 0,
    streaming: false,
    workflow_name: "wf",
    step_name: "dev",
    agent_name: "a",
  };
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

  it("sessions/loaded fills the session order newest-first and upserts jobs", () => {
    const a = jobRow("a", "2024-01-01T00:00:00Z");
    const b = jobRow("b", "2024-01-02T00:00:00Z");
    let s: AppState = { ...initialState(), activeProject: "/p" };
    s = rootReducer(s, { type: "sessions/loaded", root: "/p", jobs: [a, b] });
    expect(s.sessionOrderByProject["/p"]).toEqual(["b", "a"]); // newest first
    expect(s.jobs["a"]).toBeDefined();
    expect(s.jobs["b"]).toBeDefined();
  });

  it("sidebar panel: ui/sidebarPanel sets the active accordion panel", () => {
    let s = initialState();
    expect(s.ui.sidebarPanel).toBe("tasks"); // default
    s = rootReducer(s, { type: "ui/sidebarPanel", panel: "workflows" });
    expect(s.ui.sidebarPanel).toBe("workflows");
  });

  it("sidebar panel: selecting an entity auto-expands the matching panel", () => {
    let s = initialState();
    s = rootReducer(s, { type: "selection/set", selection: { kind: "session", jobId: "j1" } });
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

  it("job/upsert prepends a new job to the active project's session order, idempotently", () => {
    const a = jobRow("a", "2024-01-01T00:00:00Z");
    let s: AppState = {
      ...initialState(),
      activeProject: "/p",
      jobs: { a },
      sessionOrderByProject: { "/p": ["a"] },
    };
    const c = jobRow("c", "2024-01-03T00:00:00Z");
    s = rootReducer(s, { type: "job/upsert", job: c });
    expect(s.sessionOrderByProject["/p"]).toEqual(["c", "a"]);
    // Re-upserting an existing job updates the map but never duplicates the order.
    s = rootReducer(s, { type: "job/upsert", job: { ...c, status: "done" } });
    expect(s.sessionOrderByProject["/p"]).toEqual(["c", "a"]);
    expect(s.jobs["c"].status).toBe("done");
  });
});
