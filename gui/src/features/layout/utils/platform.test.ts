import { afterEach, describe, expect, it, vi } from "vitest";
import { isMacPlatform, isTauriRuntime, isWindowsPlatform, platformKind } from "./platform";

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubNavigator(value: unknown) {
  vi.stubGlobal("navigator", value);
}

describe("platformKind", () => {
  it("maps macOS", () => {
    stubNavigator({ platform: "MacIntel" });
    expect(platformKind()).toBe("mac");
    expect(isMacPlatform()).toBe(true);
    expect(isWindowsPlatform()).toBe(false);
  });

  it("maps Windows", () => {
    stubNavigator({ platform: "Win32" });
    expect(platformKind()).toBe("windows");
    expect(isWindowsPlatform()).toBe(true);
    expect(isMacPlatform()).toBe(false);
  });

  it("maps Linux", () => {
    stubNavigator({ platform: "Linux x86_64" });
    expect(platformKind()).toBe("linux");
  });

  it("prefers userAgentData.platform over navigator.platform", () => {
    stubNavigator({ platform: "MacIntel", userAgentData: { platform: "Windows" } });
    expect(platformKind()).toBe("windows");
  });

  it("returns unknown without navigator", () => {
    stubNavigator(undefined);
    expect(platformKind()).toBe("unknown");
  });
});

describe("isTauriRuntime", () => {
  it("is false without a Tauri window", () => {
    expect(isTauriRuntime()).toBe(false);
  });

  it("is true when __TAURI_INTERNALS__ is present", () => {
    vi.stubGlobal("window", { __TAURI_INTERNALS__: {} });
    expect(isTauriRuntime()).toBe(true);
  });
});
