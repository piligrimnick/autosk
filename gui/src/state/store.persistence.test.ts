// @vitest-environment jsdom
//
// Integration tests for `StoreProvider` itself (ask-8c2aee), not the pure
// reducer/helpers in isolation. Two things can only be proven by mounting the
// real component and letting its effects run:
//
//   AC14/AC15 — the state-driven `useEffect(() => saveActiveProject(state.
//   activeProject), [state.activeProject])` actually exists and actually fires
//   on explicit selection AND on every reducer-resolved fallback. reducer.test.ts
//   and activeProject.test.ts each prove one half; neither proves the two are
//   wired together inside StoreProvider.
//
//   The stale-hydrated-root bug found in review (store.tsx:546): the
//   activeProject-effect must not open (`task.subscribe` / `session.
//   subscribeProject`) a root that was hydrated from storage but has since been
//   removed from the registry — that effect must wait for `projects/loaded` to
//   resolve/validate `activeProject` before subscribing, or a stale project
//   gets opened on the daemon and its tasks can start running before the UI
//   ever shows it as selected.
//
// human explicitly approved adding `jsdom` as a devDependency for this file
// (task ask-8c2aee comment thread) after two rounds of test-review discussion
// on whether transitive coverage was sufficient — it was judged insufficient
// for a correctness bug that lives inside the component's effects.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot, type Root } from "react-dom/client";
import { act, createElement } from "react";
import type { ProjectInfo } from "@/types";
import type { Action, AppState } from "./types";

vi.mock("@/features/projects/utils/activeProject", () => ({
  ACTIVE_PROJECT_STORAGE_KEY: "autosk.activeProject",
  loadActiveProject: vi.fn(() => null),
  saveActiveProject: vi.fn(),
}));

vi.mock("@/services/events", () => ({
  subscribeDaemonStatus: vi.fn(() => () => {}),
  subscribeProjectChanged: vi.fn(() => () => {}),
  subscribeSessionChanged: vi.fn(() => () => {}),
  subscribeTaskChanged: vi.fn(() => () => {}),
  subscribeSessionEvents: vi.fn(() => () => {}),
}));

vi.mock("@/services/ipc", () => ({
  getAppSettings: vi.fn(() => Promise.resolve({})),
  getDaemonStatus: vi.fn(() => Promise.resolve({ connected: true, mode: "local" })),
  reconnectDaemon: vi.fn(() => Promise.resolve({ connected: true, mode: "local" })),
  projectList: vi.fn(() => Promise.resolve([])),
  projectDiagnostics: vi.fn(() => Promise.resolve({ errors: [] })),
  taskSubscribe: vi.fn(() => Promise.resolve({ ok: true })),
  taskUnsubscribe: vi.fn(() => Promise.resolve({ ok: true })),
  taskList: vi.fn(() => Promise.resolve([])),
  taskGet: vi.fn(() => Promise.reject(new Error("not used in this test"))),
  sessionSubscribeProject: vi.fn(() => Promise.resolve({ ok: true })),
  sessionUnsubscribeProject: vi.fn(() => Promise.resolve({ ok: true })),
  sessionSubscribe: vi.fn(() => Promise.resolve({ ok: true })),
  sessionUnsubscribe: vi.fn(() => Promise.resolve({ ok: true })),
  sessionList: vi.fn(() => Promise.resolve([])),
  sessionTranscript: vi.fn(() => Promise.reject(new Error("not used in this test"))),
  workflowList: vi.fn(() => Promise.resolve([])),
  commentList: vi.fn(() => Promise.reject(new Error("not used in this test"))),
}));

import * as ipc from "@/services/ipc";
import { loadActiveProject, saveActiveProject } from "@/features/projects/utils/activeProject";
import { StoreProvider, useStore } from "./store";

function project(root: string): ProjectInfo {
  return { root, name: root };
}

/** Lets a test control exactly when `projectList()` resolves, so it can
 *  observe state while `activeProject` is still the raw hydrated value (before
 *  `projects/loaded` has had a chance to validate/fall back). */
function deferredProjectList() {
  let resolve!: (projects: ProjectInfo[]) => void;
  const promise = new Promise<ProjectInfo[]>((res) => {
    resolve = res;
  });
  vi.mocked(ipc.projectList).mockReturnValue(promise);
  return (projects: ProjectInfo[]) => resolve(projects);
}

/** Flushes the microtask queue inside `act` so chained `await`s in the
 *  effects (projectList -> dispatch -> re-render -> next effect) settle. */
async function flush() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

// react-dom/test-utils' act() only suppresses its "not configured to support
// act" warning when this global is set; it has no other test behavior effect.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let container: HTMLDivElement;
let root: Root;
let captured: { state: AppState; dispatch: (a: Action) => void } | null;

function Probe() {
  captured = useStore();
  return null;
}

function mount() {
  act(() => {
    root.render(createElement(StoreProvider, null, createElement(Probe)));
  });
}

beforeEach(() => {
  vi.mocked(loadActiveProject).mockReturnValue(null);
  vi.mocked(saveActiveProject).mockClear();
  vi.mocked(ipc.taskSubscribe).mockClear();
  vi.mocked(ipc.sessionSubscribeProject).mockClear();
  vi.mocked(ipc.projectList).mockReset();
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
  captured = null;
});

afterEach(() => {
  act(() => {
    root.unmount();
  });
  container.remove();
});

describe("StoreProvider active-project persistence (AC14/AC15)", () => {
  it("AC14: persists an explicit project selection", async () => {
    vi.mocked(ipc.projectList).mockResolvedValue([project("/a"), project("/b")]);
    mount();
    await flush();
    expect(captured?.state.activeProject).toBe("/a"); // reducer auto-selects the first entry

    act(() => {
      captured!.dispatch({ type: "project/select", root: "/b" });
    });
    await flush();

    expect(saveActiveProject).toHaveBeenCalledWith("/b");
  });

  it("AC15: persists the reducer's removed-project fallback, not the stale hydrated root", async () => {
    vi.mocked(loadActiveProject).mockReturnValue("/gone");
    const resolveProjects = deferredProjectList();
    mount();
    // At this point activeProject is still the raw hydrated value; the
    // registry has not loaded yet.
    expect(captured?.state.activeProject).toBe("/gone");

    resolveProjects([project("/a")]); // "/gone" is no longer registered
    await flush();

    expect(captured?.state.activeProject).toBe("/a");
    // The architect's contract explicitly allows one transient write of the
    // stale hydrated root before the registry resolves (store.tsx's effect
    // has no way to know it's stale yet); what AC15 requires is that the
    // *final* persisted value is the reducer-resolved one, not that "/gone"
    // is never touched.
    const calls = vi.mocked(saveActiveProject).mock.calls;
    expect(calls[calls.length - 1]?.[0]).toBe("/a");
  });

  it("AC15: persists null when the registry loads empty", async () => {
    vi.mocked(loadActiveProject).mockReturnValue("/z");
    vi.mocked(ipc.projectList).mockResolvedValue([]);
    mount();
    await flush();

    expect(captured?.state.activeProject).toBeNull();
    const calls = vi.mocked(saveActiveProject).mock.calls;
    expect(calls[calls.length - 1]?.[0]).toBeNull();
  });
});

describe("StoreProvider active-project subscribe gating (review bug: stale hydrated root)", () => {
  it("never opens task.subscribe/session.subscribeProject for a hydrated root that the registry has removed", async () => {
    vi.mocked(loadActiveProject).mockReturnValue("/gone");
    const resolveProjects = deferredProjectList();
    mount(); // synchronously flushes the mount-time effects, including any
    // (buggy) subscribe call fired before the registry has validated
    // the hydrated root.

    resolveProjects([project("/a")]); // "/gone" is no longer registered
    await flush();

    expect(captured?.state.activeProject).toBe("/a");

    const subscribedRoots = vi.mocked(ipc.taskSubscribe).mock.calls.map(([root]) => root);
    const projectSubscribedRoots = vi.mocked(ipc.sessionSubscribeProject).mock.calls.map(([root]) => root);
    expect(subscribedRoots).not.toContain("/gone");
    expect(projectSubscribedRoots).not.toContain("/gone");
    // The correctly-resolved project must still get subscribed.
    expect(subscribedRoots).toContain("/a");
    expect(projectSubscribedRoots).toContain("/a");
  });

  it("still opens task.subscribe for a hydrated root that remains valid, once the registry confirms it", async () => {
    vi.mocked(loadActiveProject).mockReturnValue("/a");
    const resolveProjects = deferredProjectList();
    mount();

    resolveProjects([project("/a"), project("/b")]);
    await flush();

    expect(captured?.state.activeProject).toBe("/a");
    const subscribedRoots = vi.mocked(ipc.taskSubscribe).mock.calls.map(([root]) => root);
    expect(subscribedRoots).toContain("/a");
  });
});
