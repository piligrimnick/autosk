import { describe, expect, test } from "bun:test";
import { singleStep } from "../src/index.ts";

describe("singleStep", () => {
  test("materialises a single:<agent> workflow with one `do` step", () => {
    const wf = singleStep("dev");
    expect(wf.name).toBe("single:dev");
    expect(wf.firstStep).toBe("do");
    expect(wf.steps).toEqual({ do: { agent: "dev" } });
    expect(wf.isolation).toBeUndefined();
  });

  test("threads the agent name through", () => {
    expect(singleStep("@autosk/pi-agent/review").steps.do?.agent).toBe(
      "@autosk/pi-agent/review",
    );
  });
});
