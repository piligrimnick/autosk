import { isSafeExternalUrl, openExternal } from "@/services/opener";
import { popupExternalLinkMenu } from "./linkContextMenu";

export interface MarkdownLinkClick {
  preventDefault(): void;
  /** Pointer button (0 primary, 1 middle, 2 right, 3/4 back/forward). */
  button?: number;
}

export interface MarkdownLinkContextMenu {
  preventDefault(): void;
  clientX: number;
  clientY: number;
}

function isInternalNavigation(href: string): boolean {
  // Relative paths, query strings, and fragments are left untouched: they carry
  // no scheme, are not external, and are handled exactly as they were before
  // this handler existed. Protocol-relative URLs (`//host`) are external but
  // lack an explicitly validated scheme, so they are blocked below. Backslashes
  // are normalized first because a WebView rewrites e.g. `\\host/p` to
  // `//host/p` and would navigate off-site otherwise.
  const normalized = href.replace(/\\/g, "/");
  return !normalized.startsWith("//") && !/^[a-z][a-z\d+.-]*:/i.test(normalized);
}

/** Keep Markdown links inside the WebView unless they are validated HTTP(S). */
export function handleMarkdownLinkClick(event: MarkdownLinkClick, href?: string): void {
  // Only primary (0) and middle (1) clicks activate a link. onAuxClick also
  // fires for right-click (2) and mouse back/forward (3/4); those must fall
  // through untouched so the context menu and history navigation keep working
  // instead of silently opening the link in the system browser.
  if (event.button !== undefined && event.button !== 0 && event.button !== 1) {
    return;
  }

  if (!href) {
    event.preventDefault();
    return;
  }

  if (isSafeExternalUrl(href)) {
    event.preventDefault();
    void openExternal(href);
    return;
  }

  if (!isInternalNavigation(href)) {
    event.preventDefault();
  }
}

/**
 * Right-click on a validated external link pops the "Open in Browser" menu.
 * The click handler ignores button 2 so right-click does not activate the
 * link, and the WebView's own context menu has no working "Open Link" entry —
 * without this menu a right-click would be a dead end. Internal and unsafe
 * hrefs fall through to the WebView's default context-menu behavior.
 */
export function handleMarkdownLinkContextMenu(event: MarkdownLinkContextMenu, href?: string): void {
  if (!href || !isSafeExternalUrl(href)) {
    return;
  }

  event.preventDefault();
  const { clientX, clientY } = event;
  void popupExternalLinkMenu(href, clientX, clientY).catch(() => {
    // No native menu plugin (mobile) or a popup failure — the href is already
    // validated, so opening it directly beats leaving the gesture a dead end.
    openExternal(href).catch((error) => console.error(error));
  });
}
