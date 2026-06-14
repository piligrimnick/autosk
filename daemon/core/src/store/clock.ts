/**
 * Injectable wall-clock for the file store (plan §3.7(2)).
 *
 * On-disk timestamps are RFC3339 UTC with whole-second precision
 * (`YYYY-MM-DDTHH:MM:SSZ`) — the v1 contract (`time.Unix(u, 0).UTC()`) and the
 * format the design doc spells out
 * (`docs/plans/20260612-Bun-Daemon-Extensions.md` §3.1). Sub-second precision is
 * deliberately dropped so the wire/disk shape stays byte-stable across runtimes.
 *
 * The store takes a `Clock` so golden + reconciliation tests can pin time; the
 * default reads the real wall clock.
 */

/** Returns the current time as an RFC3339 UTC, whole-second string. */
export type Clock = () => string;

/** Formats a `Date` as RFC3339 UTC with whole-second precision. */
export function rfc3339Utc(d: Date): string {
  // `toISOString()` is always `YYYY-MM-DDTHH:MM:SS.mmmZ`; drop the millis.
  return d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** The default clock: real wall time, whole-second RFC3339 UTC. */
export const systemClock: Clock = () => rfc3339Utc(new Date());
