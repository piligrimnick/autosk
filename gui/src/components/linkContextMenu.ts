// linkContextMenu — the native "Open in Browser" context menu for a validated
// external Markdown link, popped at the cursor on right-click. Mirrors
// TaskRowMenu's native-menu pattern (@tauri-apps/api/menu). The Tauri WebView's
// default context menu has no working "Open Link" entry, so without this menu a
// right-click on an external link would be a dead end: the click handler
// deliberately ignores button 2 to avoid activating the link on right-click.

import { Menu, MenuItem, PredefinedMenuItem } from "@tauri-apps/api/menu";
import { LogicalPosition } from "@tauri-apps/api/dpi";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { openExternal } from "@/services/opener";

// Long URLs are truncated in the disabled header entry only; the full href is
// what "Open in Browser" hands to the validated opener.
const LABEL_MAX = 60;

export async function popupExternalLinkMenu(href: string, x: number, y: number): Promise<void> {
  const label = href.length > LABEL_MAX ? `${href.slice(0, LABEL_MAX - 1)}…` : href;
  const menu = await Menu.new({
    items: [
      await MenuItem.new({ text: label, enabled: false }),
      await PredefinedMenuItem.new({ item: "Separator" }),
      await MenuItem.new({
        text: "Open in Browser",
        action: () => void openExternal(href).catch((error) => console.error(error)),
      }),
    ],
  });
  await menu.popup(new LogicalPosition(x, y), getCurrentWindow());
}
