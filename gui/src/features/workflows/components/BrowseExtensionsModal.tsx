// BrowseExtensionsModal — the "Browse extensions" picker opened from the ＋ in
// the Workflows panel header. On open it runs the npm search (Rust command) and
// the project's `extension.list` (daemon RPC) in parallel, then renders the
// packages sorted by weekly downloads (the Rust side already sorts), flagging
// already-installed ones. Loading / empty / network-error states are handled
// inline. Picking Install opens the InstallScopeModal.

import { useEffect, useMemo, useState } from "react";
import * as ipc from "@/services/ipc";
import { Modal } from "@/components/Modal";
import type { ExtensionEntryInfo, NpmExtension } from "@/types";
import { installedScopes, type InstalledScope } from "../extensions";
import { ExtensionRow } from "./ExtensionRow";
import { InstallScopeModal } from "./InstallScopeModal";

interface BrowseExtensionsModalProps {
  cwd: string;
  onClose: () => void;
}

type LoadState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; extensions: NpmExtension[]; entries: ExtensionEntryInfo[] };

export function BrowseExtensionsModal({ cwd, onClose }: BrowseExtensionsModalProps) {
  const [load, setLoad] = useState<LoadState>({ status: "loading" });
  const [installing, setInstalling] = useState<NpmExtension | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoad({ status: "loading" });
    (async () => {
      try {
        // The npm search runs locally (Rust); extension.list goes to the daemon.
        // A failed extension.list shouldn't blank the list — fall back to empty.
        const [extensions, listing] = await Promise.all([
          ipc.extensionSearch(),
          ipc.extensionList(cwd).catch(() => ({ entries: [] as ExtensionEntryInfo[] })),
        ]);
        if (cancelled) return;
        setLoad({ status: "ready", extensions, entries: listing.entries });
      } catch (e) {
        if (cancelled) return;
        setLoad({ status: "error", message: String((e as Error).message ?? e) });
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [cwd]);

  const installed = useMemo<Map<string, InstalledScope>>(
    () => (load.status === "ready" ? installedScopes(load.entries) : new Map<string, InstalledScope>()),
    [load],
  );

  // Render only ONE Modal at a time. The shared Modal closes on Escape via a
  // window-level listener with no modal stack, so mounting both at once would
  // make Escape collapse the whole browser instead of just the scope modal.
  // While installing, the browse Modal is unmounted (its state is preserved on
  // this component) and restored when the scope modal is dismissed.
  if (installing) {
    return (
      <InstallScopeModal
        cwd={cwd}
        ext={installing}
        onClose={() => setInstalling(null)}
        onInstalled={() => {
          setInstalling(null);
          onClose();
        }}
      />
    );
  }

  return (
    <Modal title="Browse extensions" onClose={onClose}>
      {load.status === "loading" && <div className="hint">Searching npm…</div>}

      {load.status === "error" && (
        <div className="form-error">Could not search npm: {load.message}</div>
      )}

      {load.status === "ready" && load.extensions.length === 0 && (
        <p className="hint">
          No <code>autosk-extension</code> packages found. Freshly published packages can take a few
          minutes to appear in the npm search index.
        </p>
      )}

      {load.status === "ready" && load.extensions.length > 0 && (
        <ul className="ext-list">
          {load.extensions.map((ext) => (
            <ExtensionRow
              key={ext.name}
              ext={ext}
              installedScope={installed.get(ext.name) ?? null}
              onInstall={(e) => setInstalling(e)}
            />
          ))}
        </ul>
      )}
    </Modal>
  );
}
