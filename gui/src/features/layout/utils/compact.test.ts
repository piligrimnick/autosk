import { describe, expect, it } from "vitest";
import { isCompactViewport } from "./compact";

// The activation matrix from the iPhone compact-layout plan (decision 3C):
// compact engages only on TOUCH devices that are small in at least one axis.
describe("isCompactViewport", () => {
  it("is true for an iPhone in portrait (390x844, coarse)", () => {
    expect(isCompactViewport({ width: 390, height: 844, coarsePointer: true })).toBe(true);
  });

  it("is true for an iPhone in landscape (844x390, coarse) via the height clause", () => {
    expect(isCompactViewport({ width: 844, height: 390, coarsePointer: true })).toBe(true);
  });

  it("is false for an iPad in portrait (768x1024, coarse)", () => {
    expect(isCompactViewport({ width: 768, height: 1024, coarsePointer: true })).toBe(false);
  });

  it("is false for an iPad in landscape (1024x768, coarse)", () => {
    expect(isCompactViewport({ width: 1024, height: 768, coarsePointer: true })).toBe(false);
  });

  it("is false for a narrow desktop window (600x800, fine pointer)", () => {
    expect(isCompactViewport({ width: 600, height: 800, coarsePointer: false })).toBe(false);
  });
});
