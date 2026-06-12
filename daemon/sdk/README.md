# @autosk/sdk

Public, extension-facing types for autoskd v2. An autosk extension is a
default-export factory that receives an `AutoskAPI` and registers workflows and
agents:

```ts
import type { AutoskAPI } from "@autosk/sdk";

export default function (autosk: AutoskAPI) {
  autosk.registerAgent({
    name: "echo",
    async onRun(ctx) {
      await ctx.comment("hello from echo");
      await ctx.transit({ status: "done" });
    },
  });
  autosk.registerWorkflow({
    name: "trivial",
    firstStep: "do",
    steps: { do: { agent: "echo" } },
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

It also ships the `singleStep(agentName)` workflow factory and the shared id
helpers (`newTaskId`, `newSessionId`, `newEntryId`).
