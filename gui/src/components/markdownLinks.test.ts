import { beforeEach, describe, expect, it, vi } from "vitest";

const { openUrlMock, popupMenuMock } = vi.hoisted(() => ({
  openUrlMock: vi.fn(() => Promise.resolve()),
  popupMenuMock: vi.fn(() => Promise.resolve()),
}));
vi.mock("@tauri-apps/plugin-opener", () => ({ openUrl: openUrlMock }));
vi.mock("./linkContextMenu", () => ({ popupExternalLinkMenu: popupMenuMock }));

import { handleMarkdownLinkClick, handleMarkdownLinkContextMenu } from "./markdownLinks";

function click(href?: string, button?: number) {
  const event = { preventDefault: vi.fn(), button };
  handleMarkdownLinkClick(event, href);
  return event;
}

function contextMenu(href?: string) {
  const event = { preventDefault: vi.fn(), clientX: 12, clientY: 34 };
  handleMarkdownLinkContextMenu(event, href);
  return event;
}

describe("Markdown link handling", () => {
  beforeEach(() => {
    openUrlMock.mockClear();
    popupMenuMock.mockClear();
  });

  it.each([
    ["https://example.com/docs", "https://example.com/docs"],
    ["http://example.com/docs", "http://example.com/docs"],
    // No path and mixed case: assert the canonical form actually reaches the
    // opener so this layer's canonicalization is covered, not just coincidence.
    ["https://example.com", "https://example.com/"],
    ["HTTPS://EXAMPLE.COM/docs", "https://example.com/docs"],
  ])("opens %s externally as %s and prevents WebView navigation", (href, canonical) => {
    const event = click(href);

    expect(event.preventDefault).toHaveBeenCalledOnce();
    expect(openUrlMock).toHaveBeenCalledOnce();
    expect(openUrlMock).toHaveBeenCalledWith(canonical);
  });

  it.each([
    "javascript:alert(1)",
    "data:text/html,unsafe",
    "file:///tmp/secret",
    "//example.com/path",
    // Backslash variants a WebView would normalize to `//example.com`.
    "\\\\example.com/path",
    "/\\example.com",
  ])(
    "blocks unsupported URL %s",
    (href) => {
      const event = click(href);

      expect(event.preventDefault).toHaveBeenCalledOnce();
      expect(openUrlMock).not.toHaveBeenCalled();
    },
  );

  it.each([
    ["primary click (0)", 0],
    ["middle click (1)", 1],
  ])("opens an external link on %s", (_label, button) => {
    const event = click("https://example.com/docs", button);

    expect(event.preventDefault).toHaveBeenCalledOnce();
    expect(openUrlMock).toHaveBeenCalledWith("https://example.com/docs");
  });

  it.each([
    ["right click (2)", 2],
    ["mouse back (3)", 3],
    ["mouse forward (4)", 4],
  ])("ignores %s so the link is not opened or blocked", (_label, button) => {
    const event = click("https://example.com/docs", button);

    expect(event.preventDefault).not.toHaveBeenCalled();
    expect(openUrlMock).not.toHaveBeenCalled();
  });

  it.each(["#details", "/tasks/ask-123", "./next", "../previous", "?pane=detail"])(
    "leaves internal navigation %s in the app",
    (href) => {
      const event = click(href);

      expect(event.preventDefault).not.toHaveBeenCalled();
      expect(openUrlMock).not.toHaveBeenCalled();
    },
  );

  it("pops the context menu at the cursor for a validated external link", () => {
    const event = contextMenu("https://example.com/docs");

    expect(event.preventDefault).toHaveBeenCalledOnce();
    expect(popupMenuMock).toHaveBeenCalledOnce();
    expect(popupMenuMock).toHaveBeenCalledWith("https://example.com/docs", 12, 34);
    // The menu only offers the action; nothing opens until the user picks it.
    expect(openUrlMock).not.toHaveBeenCalled();
  });

  it.each([
    ["internal path", "/tasks/ask-123"],
    ["fragment", "#details"],
    ["unsafe scheme", "javascript:alert(1)"],
    ["protocol-relative URL", "//example.com/path"],
    ["missing href", undefined],
  ])("leaves the default context menu for %s", (_label, href) => {
    const event = contextMenu(href);

    expect(event.preventDefault).not.toHaveBeenCalled();
    expect(popupMenuMock).not.toHaveBeenCalled();
    expect(openUrlMock).not.toHaveBeenCalled();
  });

  it("falls back to opening the validated link when the native menu is unavailable", async () => {
    popupMenuMock.mockRejectedValueOnce(new Error("plugin menu not found"));

    const event = contextMenu("HTTPS://EXAMPLE.COM/docs");

    expect(event.preventDefault).toHaveBeenCalledOnce();
    await vi.waitFor(() => {
      expect(openUrlMock).toHaveBeenCalledWith("https://example.com/docs");
    });
  });
});
