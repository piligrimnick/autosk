/**
 * @autosk/pi-agent — placeholder for P1.
 *
 * P6 ports v1's "standard branch" here: spawn `pi --mode rpc`, drive it over
 * JSON-lines stdio via `ctx.spawn` (`ChildHandle`), mirror pi session entries
 * into `ctx.log`, register an `autosk_transit` pi-tool and bridge its calls to
 * `ctx.transit`, and reimplement the kickback loop as this extension's private
 * logic. For now this is an inert extension factory so the package typechecks
 * and the workspace wiring stays exercised.
 */

import type { AutoskAPI } from "@autosk/sdk";

export interface PiAgentOptions {
  /** Agent name to register (e.g. `"@autosk/pi-agent/dev"`). */
  name: string;
  /** pi model spec, e.g. `"sonnet:high"`. */
  model?: string;
  /** Path to a file whose contents seed the first message. */
  firstMessageFile?: string;
  /** Extra args forwarded to `pi`. */
  extraArgs?: string[];
}

/**
 * Build the pi-backed agent definition. P6 implements `onRun`; for now this
 * throws if actually run so a half-wired daemon fails loudly rather than
 * silently no-op'ing.
 */
export function piAgent(_opts: PiAgentOptions): never {
  throw new Error("@autosk/pi-agent is not implemented until P6");
}

export default function piAgentExtension(_autosk: AutoskAPI): void {
  // P6: autosk.registerAgent(piAgent({ ... })) for the dev/review/etc. roles.
}
