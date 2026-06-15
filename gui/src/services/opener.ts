// services/opener.ts — the single wrapper over the Tauri opener plugin. The
// IPC-discipline guard only flags `invoke` imported from `@tauri-apps/api`, so
// `openUrl` (from the separate `@tauri-apps/plugin-opener` module) is not
// flagged; keeping it behind one `openExternal(url)` helper mirrors the ipc.ts
// chokepoint discipline and keeps call sites transport-agnostic. The opener
// capability is scoped to https://www.npmjs.com/* in capabilities/default.json.

import { openUrl } from "@tauri-apps/plugin-opener";

/** Open an external URL in the user's default browser. */
export function openExternal(url: string): Promise<void> {
  return openUrl(url);
}
