// services/opener.ts — the single wrapper over the Tauri opener plugin. The
// IPC-discipline guard only flags `invoke` imported from `@tauri-apps/api`, so
// `openUrl` (from the separate `@tauri-apps/plugin-opener` module) is not
// flagged; keeping it behind one `openExternal(url)` helper mirrors the ipc.ts
// chokepoint discipline and keeps call sites transport-agnostic. The opener
// capability is scoped to HTTP(S) URLs in capabilities/default.json. Keep the
// same scheme validation here so untrusted rendered content cannot ask the
// native opener to handle another protocol.

import { openUrl } from "@tauri-apps/plugin-opener";

function parseSafeExternalUrl(url: string): URL | null {
  try {
    const parsed = new URL(url);
    return (parsed.protocol === "http:" || parsed.protocol === "https:") && parsed.hostname.length > 0
      ? parsed
      : null;
  } catch {
    return null;
  }
}

/** Return whether a URL is absolute HTTP(S) and safe to pass to the opener. */
export function isSafeExternalUrl(url: string): boolean {
  return parseSafeExternalUrl(url) !== null;
}

/** Open a validated external URL in the user's default browser. */
export function openExternal(url: string): Promise<void> {
  const parsed = parseSafeExternalUrl(url);
  if (!parsed) {
    return Promise.reject(new Error(`Refusing to open unsupported external URL: ${url}`));
  }
  return openUrl(parsed.href);
}
