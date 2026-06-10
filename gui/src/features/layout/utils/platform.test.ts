import { afterEach, describe, expect, it, vi } from "vitest";
import {
  isMacPlatform,
  isTauriRuntime,
  isWebviewZoomSupported,
  isWindowsPlatform,
  platformKind,
} from "./platform";

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

describe("isWebviewZoomSupported", () => {
  it("is true on Windows (incl. touch PCs)", () => {
    stubNavigator({ platform: "Win32", maxTouchPoints: 10 });
    expect(isWebviewZoomSupported()).toBe(true);
  });

  it("is true on a real (non-touch) Mac", () => {
    stubNavigator({ platform: "MacIntel", maxTouchPoints: 0 });
    expect(isWebviewZoomSupported()).toBe(true);
  });

  it("is false on an iPad that masquerades as a Mac (touch)", () => {
    stubNavigator({ platform: "MacIntel", maxTouchPoints: 5 });
    expect(isWebviewZoomSupported()).toBe(false);
  });

  it("is false on Linux / Android", () => {
    stubNavigator({ platform: "Linux x86_64" });
    expect(isWebviewZoomSupported()).toBe(false);
  });

  it("is false without a navigator", () => {
    stubNavigator(undefined);
    expect(isWebviewZoomSupported()).toBe(false);
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
