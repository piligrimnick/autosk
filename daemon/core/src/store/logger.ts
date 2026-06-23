/**
 * Minimal logger seam (plan §3.7(2) — reconciliation "warning logged").
 *
 * The store logs reconciliation rejections as warnings. A seam (instead of a
 * bare `console.warn`) lets the reconciliation tests assert "a warning was
 * logged" without scraping stderr.
 */

export interface Logger {
  info(msg: string, ...args: unknown[]): void;
  warn(msg: string, ...args: unknown[]): void;
  error(msg: string, ...args: unknown[]): void;
}

/** Logs to stderr (the default the daemon installs). */
export const consoleLogger: Logger = {
  info: (msg, ...args) => console.error(`[info] ${msg}`, ...args),
  warn: (msg, ...args) => console.error(`[warn] ${msg}`, ...args),
  error: (msg, ...args) => console.error(`[error] ${msg}`, ...args),
};

/** A logger that records every message, for tests. */
export class CapturingLogger implements Logger {
  readonly infos: string[] = [];
  readonly warns: string[] = [];
  readonly errors: string[] = [];
  info(msg: string): void {
    this.infos.push(msg);
  }
  warn(msg: string): void {
    this.warns.push(msg);
  }
  error(msg: string): void {
    this.errors.push(msg);
  }
}
