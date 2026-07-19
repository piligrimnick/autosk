// Best-effort persistence for the project selected in the desktop UI.
// Project roots remain opaque here; registry validation belongs to the reducer.

export const ACTIVE_PROJECT_STORAGE_KEY = "autosk.activeProject";

/** Load the last selected project root, if browser storage is available. */
export function loadActiveProject(): string | null {
  if (typeof window === "undefined") return null;
  try {
    const root = window.localStorage.getItem(ACTIVE_PROJECT_STORAGE_KEY);
    return root != null && root.trim().length > 0 ? root : null;
  } catch {
    return null;
  }
}

/** Persist a selected root, or clear the value when there is no selection. */
export function saveActiveProject(root: string | null): void {
  if (typeof window === "undefined") return;
  try {
    if (root == null || root.trim().length === 0) {
      window.localStorage.removeItem(ACTIVE_PROJECT_STORAGE_KEY);
      return;
    }
    window.localStorage.setItem(ACTIVE_PROJECT_STORAGE_KEY, root);
  } catch {
    /* private mode / quota — non-fatal */
  }
}
