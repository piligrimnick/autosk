// Platform detection for the frameless window chrome (redesign plan §4). Pure
// and dependency-free: safe to call outside a browser (returns "unknown" when
// `navigator` is absent), so the unit tests run under the node vitest env.

type PlatformKind = "mac" | "windows" | "linux" | "unknown";

export function platformKind(): PlatformKind {
  if (typeof navigator === "undefined") return "unknown";
  const raw =
    (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform ??
    navigator.platform ??
    "";
  const n = raw.toLowerCase();
  if (n.includes("mac")) return "mac";
  if (n.includes("win")) return "windows";
  if (n.includes("linux")) return "linux";
  return "unknown";
}

export function isMacPlatform(): boolean {
  return platformKind() === "mac";
}

export function isWindowsPlatform(): boolean {
  return platformKind() === "windows";
}

/**
 * True when running inside the Tauri webview. Tauri v2 injects
 * `window.__TAURI_INTERNALS__`; checking for it avoids importing from
 * `@tauri-apps/api/core` (which the IPC-discipline guard scans).
 */
export function isTauriRuntime(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}
