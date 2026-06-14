# @autosk/sdk

Public, extension-facing types for autoskd v2. An autosk extension is a
default-export factory that receives an `AutoskAPI` and registers workflows
(their agents are inline step values — the step key is the agent name):

```ts
import { type AutoskAPI } from "@autosk/sdk";

export default function (autosk: AutoskAPI) {
  autosk.registerWorkflow({
    name: "trivial",
    firstStep: "do",
    steps: {
      // The step key "do" is the inline agent's name; registering the
      // workflow registers its agents (there is no registerAgent).
      do: {
        async onRun(ctx) {
          await ctx.comment("hello from do");
          await ctx.transit({ status: "done" });
        },
      },
    },
  });
}
```

This package contains three layers:

- **Concept model** (`workflow.ts`, `agent.ts`, `types.ts`, `api.ts`) — the
  types extensions program against (plan §3).
- **Transcript format** (`transcript.ts`) — the pi-compatible session entry
  schema autosk writes to `./.autosk/sessions/<id>.jsonl` (plan §3.2).
- **Proto-v2** (`proto.ts`) — the JSON-RPC v2 wire types and the canonical
  method / notification manifest the Go and Tauri clients mirror (plan §4).

It also ships the `statusStep(status)` step helper (build a terminal/park step
with `statusStep("done"|"cancel"|"human")`) and the shared id helpers
(`newTaskId`, `newCommentId`, `newSessionId`, `newEntryId`). Comment ids
are strings in v2 (v1's autoincrement ints die with the SQL store), and
`newCommentId` is collision-checked against a task's existing comment ids since
the id is the edit/delete key.
