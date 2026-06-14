/**
 * The daemon's reported version + build commit (plan §4 `meta.version`).
 *
 * Kept in its own module (not `index.ts`) so the RPC layer can import the
 * constant without pulling in — and cycling through — the binary bootstrap that
 * `index.ts` wires up.
 */

/**
 * The version the daemon reports over `meta.version`. Defaults to `0.0.0-dev`;
 * release builds bake the tag in via `bun build --compile --define
 * process.env.AUTOSK_VERSION='"vX.Y.Z"'` (see scripts/package-autoskd.sh), which
 * the bundler constant-folds into this expression.
 */
export const VERSION = process.env.AUTOSK_VERSION ?? "0.0.0-dev";

/** The build commit, sourced from `$AUTOSK_COMMIT` (empty when unset). */
export function commit(): string {
  return process.env.AUTOSK_COMMIT ?? "";
}
