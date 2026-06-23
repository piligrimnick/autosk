/**
 * The injected pi extension that gives the model a way to record an autosk
 * workflow transition (plan §3.4, resolved-#2).
 *
 * `@autosk/pi-agent` passes this file to the spawned `pi --mode rpc` via `-e`.
 * It registers a single tool, `autosk_transit`; the autosk-side {@link PiDriver}
 * OBSERVES the tool call on pi's RPC event stream (`tool_execution_start`) and
 * translates it into `ctx.transit(...)`. The tool itself only returns an
 * immediate ack — the real transition (and any `onTransit` rejection fed back as
 * a corrective follow-up) is driven by the autosk daemon, so core stays closed
 * and no session-scoped daemon RPC is needed.
 *
 * NB: this module is loaded by **pi's** toolchain (it imports pi's `typebox` /
 * `ExtensionAPI`), NOT by the autosk daemon, which only ever passes its PATH to
 * `pi -e`. It is therefore excluded from the autosk workspace's `tsc` typecheck.
 */

// These resolve inside pi's environment when pi loads the extension.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";

const AUTOSK_TRANSIT_PARAMS = Type.Object({
  to: Type.String({
    description: "The transition target: a sibling step name, or one of done | cancel | human.",
  }),
});

export default function autoskTransitExtension(pi: ExtensionAPI): void {
  pi.registerTool({
    name: "autosk_transit",
    label: "autosk transit",
    description:
      "Record the workflow transition for the current autosk task. Call this exactly once, when you " +
      "are done with the step, with `to` set to the chosen sibling step name or one of done | cancel | human. " +
      "If a transition is rejected you will be told and may call this again with a different target.",
    promptSnippet: "Record the chosen autosk workflow transition (call exactly once when done).",
    promptGuidelines: [
      "Call autosk_transit exactly once before you stop, with the chosen transition target.",
    ],
    parameters: AUTOSK_TRANSIT_PARAMS,
    async execute(_toolCallId, params) {
      const to = String((params as { to: string }).to ?? "").trim();
      return {
        content: [
          {
            type: "text" as const,
            text:
              to === ""
                ? "autosk: no transition target provided — call autosk_transit again with a `to`."
                : `autosk: transition to "${to}" submitted.`,
          },
        ],
      };
    },
  });
}
