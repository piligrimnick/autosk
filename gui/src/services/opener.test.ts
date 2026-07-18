import { beforeEach, describe, expect, it, vi } from "vitest";

const { openUrlMock } = vi.hoisted(() => ({ openUrlMock: vi.fn(() => Promise.resolve()) }));
vi.mock("@tauri-apps/plugin-opener", () => ({ openUrl: openUrlMock }));

import { isSafeExternalUrl, openExternal } from "./opener";

describe("external URL opener", () => {
  beforeEach(() => {
    openUrlMock.mockClear();
  });

  it.each(["https://example.com", "http://localhost:3000/path", "HTTPS://EXAMPLE.COM/docs"])(
    "accepts HTTP(S) URL %s",
    (url) => {
      expect(isSafeExternalUrl(url)).toBe(true);
    },
  );

  it("passes a canonical URL to the native opener", async () => {
    await openExternal("HTTPS://EXAMPLE.COM/docs");

    expect(openUrlMock).toHaveBeenCalledOnce();
    expect(openUrlMock).toHaveBeenCalledWith("https://example.com/docs");
  });

  it.each(["javascript:alert(1)", "data:text/plain,unsafe", "file:///tmp/secret", "/relative", "//example.com"])(
    "rejects unsupported URL %s before calling the native opener",
    async (url) => {
      expect(isSafeExternalUrl(url)).toBe(false);
      await expect(openExternal(url)).rejects.toThrow("Refusing to open unsupported external URL");
      expect(openUrlMock).not.toHaveBeenCalled();
    },
  );
});
