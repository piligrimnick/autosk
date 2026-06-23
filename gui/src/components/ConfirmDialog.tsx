// ConfirmDialog — an in-app replacement for the native `window.confirm()`,
// which does NOT work in this Tauri/wry build: on macOS WKWebView wry's
// `WKUIDelegate` implements only the file-upload / media-permission / window.open
// panels, so `runJavaScriptConfirmPanelWithMessage` is missing and the webview's
// default is to return `false` with no dialog. Every `confirm()` gate therefore
// silently bailed (the project-remove "×", comment delete, session abort,
// force-cancel). This provider renders a real React modal and resolves a
// Promise<boolean>, so confirmations work identically on every platform.
//
// Usage:
//   const confirm = useConfirm();
//   if (!(await confirm({ title: "Remove project", message: "…", danger: true }))) return;
//
// A bare string is accepted as shorthand for `{ message }`.

import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import { Modal } from "@/components/Modal";

export interface ConfirmOptions {
  /** Modal header (defaults to "Confirm"). */
  title?: string;
  /** Body text; `\n` renders as line breaks (the body is `white-space: pre-line`). */
  message: ReactNode;
  /** Primary (accept) button label (defaults to "Confirm"). */
  confirmLabel?: string;
  /** Cancel button label (defaults to "Cancel"). */
  cancelLabel?: string;
  /** Style the accept button as destructive (red) instead of primary (blue). */
  danger?: boolean;
}

type ConfirmFn = (opts: ConfirmOptions | string) => Promise<boolean>;

const ConfirmContext = createContext<ConfirmFn | null>(null);

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  // The pending promise's resolver lives in a ref so `settle` never touches it
  // from inside a state-updater (which React StrictMode double-invokes in dev).
  const resolveRef = useRef<((ok: boolean) => void) | null>(null);

  const confirm = useCallback<ConfirmFn>((optsOrMsg) => {
    const next: ConfirmOptions = typeof optsOrMsg === "string" ? { message: optsOrMsg } : optsOrMsg;
    return new Promise<boolean>((resolve) => {
      // A new request supersedes any still-open one: settle the old as declined.
      resolveRef.current?.(false);
      resolveRef.current = resolve;
      setOpts(next);
    });
  }, []);

  const settle = useCallback((ok: boolean) => {
    const resolve = resolveRef.current;
    resolveRef.current = null;
    setOpts(null);
    resolve?.(ok);
  }, []);

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      {opts && (
        <Modal
          title={opts.title ?? "Confirm"}
          onClose={() => settle(false)}
          footer={
            <>
              <button className="btn" onClick={() => settle(false)}>
                {opts.cancelLabel ?? "Cancel"}
              </button>
              <button
                className={`btn ${opts.danger ? "btn-danger" : "btn-primary"}`}
                autoFocus
                onClick={() => settle(true)}
              >
                {opts.confirmLabel ?? "Confirm"}
              </button>
            </>
          }
        >
          <div className="confirm-message">{opts.message}</div>
        </Modal>
      )}
    </ConfirmContext.Provider>
  );
}

export function useConfirm(): ConfirmFn {
  const ctx = useContext(ConfirmContext);
  if (!ctx) {
    throw new Error("useConfirm must be used within <ConfirmProvider>");
  }
  return ctx;
}
