// Test fixture extension (plain JS so it loads with no @autosk/sdk import).
//
// Registers deterministic fixtures used by the cmd/autosk verb tests:
//
//   - "human-flow": a human-only workflow. Because the first step is a
//     statusStep("human"), the daemon scheduler parks an enrolled task at
//     `human` and NEVER auto-runs an agent — so enroll/registry verb assertions
//     stay race-free without any pi/agent in the test environment.
//
//   - "auto-flow": a single-step workflow whose inline agent runs in-process,
//     writes one custom transcript entry, and transits the task to `done`. Used
//     by the end-to-end smoke (TestE2E_*) to exercise a real session +
//     transcript without needing pi. No other test enrolls into it, so it never
//     runs for the human-flow tests.
//
// Agents are inline step values now (the step key IS the agent name): a step is
// an agent definition (has `onRun`) or a statusStep (`{ status }`).
export default function (autosk) {
  autosk.registerWorkflow({
    name: "human-flow",
    description: "deterministic human-only workflow for verb tests",
    firstStep: "review",
    steps: {
      review: { status: "human" },
      accept: { status: "human" },
    },
  });

  autosk.registerWorkflow({
    name: "auto-flow",
    description: "single-step workflow driven by an in-process stub agent",
    firstStep: "do",
    steps: {
      do: {
        async onRun(ctx) {
          ctx.log.custom("note", { text: "stub agent ran" });
          await ctx.comment("stub agent finished");
          await ctx.transit({ status: "done" });
        },
      },
    },
  });
}
