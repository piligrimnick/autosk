// Test fixture extension (plain JS so it loads with no @autosk/sdk import).
//
// Registers deterministic fixtures used by the cmd/autosk verb tests:
//
//   - "human-flow": a human-only workflow. Because the first step is
//     human-owned, the daemon scheduler parks an enrolled task at `human` and
//     NEVER auto-runs an agent — so enroll/registry verb assertions stay
//     race-free without any pi/agent in the test environment.
//
//   - "auto-flow" + the "stub" agent: a single-step workflow whose agent runs
//     in-process, writes one custom transcript entry, and transits the task to
//     `done`. Used by the end-to-end smoke (TestE2E_*) to exercise a real
//     session + transcript without needing pi. No other test enrolls into it,
//     so it never runs for the human-flow tests.
export default function (autosk) {
  autosk.registerWorkflow({
    name: "human-flow",
    description: "deterministic human-only workflow for verb tests",
    firstStep: "review",
    steps: {
      review: { human: true },
      accept: { human: true },
    },
  });

  autosk.registerAgent({
    name: "stub",
    async onRun(ctx) {
      ctx.log.custom("note", { text: "stub agent ran" });
      await ctx.comment("stub agent finished");
      await ctx.transit({ status: "done" });
    },
  });

  autosk.registerWorkflow({
    name: "auto-flow",
    description: "single-step workflow driven by the in-process stub agent",
    firstStep: "do",
    steps: {
      do: { agent: "stub" },
    },
  });
}
