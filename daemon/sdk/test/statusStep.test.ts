import { describe, expect, test } from "bun:test";
import {
  isAgentStep,
  isStatusStep,
  statusStep,
  type AgentDefinition,
  type StepDef,
} from "../src/index.ts";

describe("statusStep", () => {
  test("returns a { status } step value", () => {
    expect(statusStep("human")).toEqual({ status: "human" });
    expect(statusStep("done")).toEqual({ status: "done" });
    expect(statusStep("cancel")).toEqual({ status: "cancel" });
  });
});

describe("step guards", () => {
  const agentStep: AgentDefinition = {
    async onRun() {
      /* no-op */
    },
  };

  test("isStatusStep narrows a statusStep and rejects an agent step", () => {
    const status: StepDef = statusStep("human");
    expect(isStatusStep(status)).toBe(true);
    expect(isStatusStep(agentStep)).toBe(false);
  });

  test("isAgentStep narrows an agent step and rejects a statusStep", () => {
    expect(isAgentStep(agentStep)).toBe(true);
    expect(isAgentStep(statusStep("done"))).toBe(false);
  });
});
